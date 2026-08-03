package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
)

const mcpBrokerBootTimeout = 60 * time.Second

// productionMCPRuntime is the parent-side, public-data-only handle to the
// credential-owning child. The client transports calls; inventory is the live
// names/uses-workspace snapshot consumed by scheduled scope construction.
type productionMCPRuntime struct {
	client    *mcpbroker.Client
	stop      func() error
	inventory *brokerMCPInventory
	catalog   []mcp.ServerTool
	accounts  map[string][]string
}

type brokerMCPInventory struct {
	mu      sync.RWMutex
	servers map[string]scheduledrun.TaskMCPServerInfo
}

func productionMCPBrokerCommand() (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve fleet executable for MCP broker: %w", err)
	}
	cmd := exec.CommandContext(context.Background(), executable, "mcp-broker") //nolint:gosec // executable is the running fleet binary, not request input.
	cmd.Stderr = os.Stderr
	return cmd, nil
}

func startDefaultProductionMCPRuntime(bundle *clientconfig.Bundle, cfg *config.Config) (*productionMCPRuntime, map[string]agent.MCPServerSpec, error) {
	cmd, err := productionMCPBrokerCommand()
	if err != nil {
		return nil, nil, err
	}
	return startProductionMCPRuntime(bundle, cfg, cmd)
}

func (i *brokerMCPInventory) replace(descriptors []mcpbroker.ServerDescriptor) {
	servers := make(map[string]scheduledrun.TaskMCPServerInfo, len(descriptors))
	for _, descriptor := range descriptors {
		servers[descriptor.Name] = scheduledrun.TaskMCPServerInfo{UsesWorkspace: descriptor.UsesWorkspace}
	}
	i.mu.Lock()
	i.servers = servers
	i.mu.Unlock()
}

func (i *brokerMCPInventory) snapshot() map[string]scheduledrun.TaskMCPServerInfo {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make(map[string]scheduledrun.TaskMCPServerInfo, len(i.servers))
	for name, server := range i.servers {
		out[name] = server
	}
	return out
}

// startProductionMCPRuntime starts and verifies the broker before removing any
// connector material from the parent. cmd.Env must remain nil so Start snapshots
// the current environment into the child; scrubbing happens only after Ping and
// public discovery succeed.
func startProductionMCPRuntime(bundle *clientconfig.Bundle, cfg *config.Config, cmd *exec.Cmd) (*productionMCPRuntime, map[string]agent.MCPServerSpec, error) {
	if bundle == nil || cfg == nil || cmd == nil {
		return nil, nil, errors.New("start MCP broker: bundle, config, and command are required")
	}
	if cmd.Env != nil {
		return nil, nil, errors.New("start MCP broker: command environment must be inherited")
	}
	if err := validateConnectorParentEnvSeparation(bundle); err != nil {
		return nil, nil, err
	}

	resolvedSpecs := scheduledrun.BuildMCPSpecs(cfg)
	publicSpecs := publicMCPServerSpecs(resolvedSpecs)
	client, stop, err := mcpbroker.SpawnClient(cmd)
	if err != nil {
		return nil, nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = stop()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), mcpBrokerBootTimeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return nil, nil, fmt.Errorf("start MCP broker: ping: %w", err)
	}
	descriptors, err := client.ListTools(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("start MCP broker: list tools: %w", err)
	}
	accounts := make(map[string][]string, len(publicSpecs))
	for name, spec := range publicSpecs {
		accounts[name], err = client.ListAccounts(ctx, name, spec.AccountVars)
		if err != nil {
			return nil, nil, fmt.Errorf("start MCP broker: list accounts for %s: %w", name, err)
		}
	}
	inventory := &brokerMCPInventory{servers: taskMCPInventoryFromResolvedSpecs(resolvedSpecs)}
	runtime := &productionMCPRuntime{
		client:    client,
		stop:      stop,
		inventory: inventory,
		catalog:   brokerToolCatalog(descriptors),
		accounts:  cloneAccountInventory(accounts),
	}

	if err := scrubParentConnectorState(bundle, cfg, resolvedSpecs); err != nil {
		return nil, nil, err
	}
	failed = false
	return runtime, publicSpecs, nil
}

func (r *productionMCPRuntime) Close() error { return r.stop() }

func (r *productionMCPRuntime) openInteractiveScope(ctx context.Context, selection agentcore.MCPSelection, workspace string) (*agent.MCPScope, error) {
	return r.openScope(ctx, selection, "", workspace)
}

func (r *productionMCPRuntime) openTaskScope(ctx context.Context, selection agentcore.MCPSelection, taskID, workspace string) (*agent.MCPScope, error) {
	return r.openScope(ctx, selection, taskID, workspace)
}

// openRemoteOverlay asks the credential-owning child to construct one user's
// hosted-MCP overlay. Only identity and public routing names cross the process
// boundary; connection records, URLs, and credentials remain child-side.
func (r *productionMCPRuntime) openRemoteOverlay(ctx context.Context, email string, shadowed, enabled map[string]bool) (*agent.RemoteMCPOverlay, error) {
	scope, err := r.client.OpenScope(ctx, mcpbroker.ScopeSpec{Remote: &mcpbroker.RemoteScopeSpec{
		UserEmail:     email,
		FilterEnabled: enabled != nil,
		Enabled:       sortedEnabledNames(enabled),
		Shadowed:      sortedEnabledNames(shadowed),
	}})
	if err != nil {
		return nil, err
	}
	tools := scope.Tools()
	servers := make(map[string]bool, len(tools))
	for _, tool := range tools {
		servers[tool.Server] = true
	}
	return &agent.RemoteMCPOverlay{
		Broker:     scope,
		Catalog:    brokerToolCatalog(tools),
		Servers:    servers,
		CloseScope: scope.Close,
		Skipped:    scope.Skipped(),
	}, nil
}

func sortedEnabledNames(enabled map[string]bool) []string {
	names := make([]string, 0, len(enabled))
	for name, on := range enabled {
		if on {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (r *productionMCPRuntime) openScope(ctx context.Context, selection agentcore.MCPSelection, taskID, workspace string) (*agent.MCPScope, error) {
	choices := make([]mcpbroker.ScopeChoice, 0, len(selection))
	for _, choice := range selection {
		choices = append(choices, mcpbroker.ScopeChoice{Server: choice.Server, Account: choice.Account})
	}
	scope, err := r.client.OpenScope(ctx, mcpbroker.ScopeSpec{Selection: choices, TaskID: taskID, Workspace: workspace})
	if err != nil {
		return nil, err
	}
	return &agent.MCPScope{Broker: scope, Catalog: brokerToolCatalog(scope.Tools()), Close: scope.Close}, nil
}

func (r *productionMCPRuntime) reload(ctx context.Context) (*agent.MCPReloadResult, error) {
	result, err := r.client.Reload(ctx)
	if err != nil {
		return nil, err
	}
	r.inventory.replace(result.Servers)
	return &agent.MCPReloadResult{
		Summary: mcp.ReloadSummary{
			Added:     append([]string(nil), result.Summary.Added...),
			Removed:   append([]string(nil), result.Summary.Removed...),
			Restarted: append([]string(nil), result.Summary.Restarted...),
			Unchanged: append([]string(nil), result.Summary.Unchanged...),
		},
		Catalog:  brokerToolCatalog(result.Tools),
		Accounts: cloneAccountInventory(result.Accounts),
		Specs:    publicSpecsFromDescriptors(result.Servers),
	}, nil
}

func brokerToolCatalog(descriptors []mcpbroker.ToolDescriptor) []mcp.ServerTool {
	out := make([]mcp.ServerTool, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, mcp.ServerTool{
			ServerName: descriptor.Server,
			Tool: mcp.Tool{
				Name:        descriptor.Tool,
				Description: descriptor.Description,
				InputSchema: descriptor.InputSchema,
			},
		})
	}
	return out
}

func publicSpecsFromDescriptors(descriptors []mcpbroker.ServerDescriptor) map[string]agent.MCPServerSpec {
	out := make(map[string]agent.MCPServerSpec, len(descriptors))
	for _, descriptor := range descriptors {
		out[descriptor.Name] = agent.MCPServerSpec{
			Enabled:          true,
			ToolAllowlist:    append([]string(nil), descriptor.ToolAllowlist...),
			AccountVars:      append([]string(nil), descriptor.AccountVars...),
			Optional:         descriptor.Optional,
			DisplayName:      descriptor.DisplayName,
			Description:      descriptor.Description,
			Beta:             descriptor.Beta,
			EnabledByDefault: descriptor.EnabledByDefault,
		}
	}
	return out
}

func publicMCPServerSpecs(src map[string]agent.MCPServerSpec) map[string]agent.MCPServerSpec {
	out := make(map[string]agent.MCPServerSpec, len(src))
	for name, spec := range src {
		if !spec.Enabled {
			continue
		}
		out[name] = agent.MCPServerSpec{
			Enabled:          true,
			ToolAllowlist:    append([]string(nil), spec.ToolAllowlist...),
			AccountVars:      append([]string(nil), spec.AccountVars...),
			Optional:         spec.Optional,
			DisplayName:      spec.DisplayName,
			Description:      spec.Description,
			Beta:             spec.Beta,
			EnabledByDefault: spec.EnabledByDefault,
		}
	}
	return out
}

func taskMCPInventoryFromResolvedSpecs(src map[string]agent.MCPServerSpec) map[string]scheduledrun.TaskMCPServerInfo {
	out := make(map[string]scheduledrun.TaskMCPServerInfo, len(src))
	for name, spec := range src {
		if spec.Enabled {
			out[name] = scheduledrun.TaskMCPServerInfo{UsesWorkspace: agentcore.EnvReferencesWorkspace(spec.Env)}
		}
	}
	return out
}

func cloneAccountInventory(src map[string][]string) map[string][]string {
	out := make(map[string][]string, len(src))
	for server, accounts := range src {
		out[server] = append([]string(nil), accounts...)
	}
	return out
}

func validateConnectorParentEnvSeparation(bundle *clientconfig.Bundle) error {
	parent := map[string]bool{}
	parentNames := parentOwnedRuntimeEnvNames(bundle)
	for _, name := range parentNames {
		parent[strings.ToUpper(strings.TrimSpace(name))] = true
	}
	isParent := func(name string) bool {
		upper := strings.ToUpper(strings.TrimSpace(name))
		if parent[upper] {
			return true
		}
		// CUTLASS_ is deliberately NOT a blanket parent-owned prefix: fleet's
		// own connector contract (internal/agentcore/mcp_workspace.go) hands
		// CUTLASS_RUN_WORKDIR / CUTLASS_MOC_TASK_ID / CUTLASS_REPORT_DIR /
		// CUTLASS_INPUT_DIR to cutlass-family MCP servers as bundle-declared
		// wire keys, and operators pass knobs like CUTLASS_ALLOWED_DIRS through
		// to connectors. The CUTLASS_* names the parent runtime itself resolves
		// are enumerated in parentOwnedRuntimeEnvNames instead.
		for _, prefix := range []string{"FLEET_", "CHAT_", "DB_", "OPENROUTER_"} {
			if strings.HasPrefix(upper, prefix) {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	var overlap []string
	for _, name := range bundle.ConnectorEnvVarNames() {
		if isParent(name) && !seen[name] {
			seen[name] = true
			overlap = append(overlap, name)
		}
	}
	for _, base := range bundle.ConnectorAccountEnvVarNames() {
		if isParent(base) && !seen[base] {
			seen[base] = true
			overlap = append(overlap, base)
		}
		prefix := strings.ToUpper(strings.TrimSpace(base)) + "_"
		for _, parentName := range parentNames {
			upper := strings.ToUpper(strings.TrimSpace(parentName))
			if strings.HasPrefix(upper, prefix) && validAccountEnvSuffix(strings.TrimPrefix(upper, prefix)) && !seen[parentName] {
				seen[parentName] = true
				overlap = append(overlap, parentName)
			}
		}
	}
	// Include concrete account-suffixed keys that this boot would scrub. Their
	// base is a connector subprocess env key rather than necessarily a ${VAR}
	// source name, so the raw-reference inventory alone cannot detect overlap.
	for _, name := range bundle.ConnectorEnvironmentKeys(os.Environ()) {
		if isParent(name) && !seen[name] {
			seen[name] = true
			overlap = append(overlap, name)
		}
	}
	sort.Strings(overlap)
	if len(overlap) > 0 {
		return fmt.Errorf("connector environment overlaps parent-owned configuration: %s", strings.Join(overlap, ", "))
	}
	return nil
}

func validAccountEnvSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	for _, r := range suffix {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func parentOwnedRuntimeEnvNames(bundle *clientconfig.Bundle) []string {
	names := []string{
		"FLEET_ENV_FILE", clientconfig.EnvDir,
		"OPENROUTER_API_KEY", "FLEET_SERVER_TOKEN", "CHAT_SERVER_TOKEN", "ADMIN_API_KEY",
		"DATABASE_URL", "FLEET_CHAT_DATABASE_URL", "FLEET_SCHED_DATABASE_URL", "DB_PASSWORD",
		"TAVILY_API_KEY", "FLEET_SMTP_PASSWORD", "FLEET_WEBHOOK_SECRET", "FLEET_VAPID_PRIVATE_KEY",
		"FLEET_MCP_OAUTH_ENCRYPTION_KEY", "CHAT_MCP_OAUTH_ENCRYPTION_KEY", "CUTLASS_MCP_OAUTH_ENCRYPTION_KEY",
		"FLEET_LOG_ARCHIVE_ENCRYPTION_KEY", "CHAT_LOG_ARCHIVE_ENCRYPTION_KEY", "CUTLASS_LOG_ARCHIVE_ENCRYPTION_KEY",
		// Legacy CUTLASS_* names the parent runtime itself resolves (boot config,
		// per-run lookups through the EnvPrefix alias machinery, or lazy os.Getenv
		// reads) — enumerated explicitly because the CUTLASS_ prefix as a whole is
		// shared with the cutlass-family connector wire contract and must stay
		// claimable by bundles (see the isParent comment above).
		"CUTLASS_TASK_MODEL", "CUTLASS_TASK_FALLBACK_MODEL", "CUTLASS_TASK_MAX_ITERATIONS",
		"CUTLASS_INPUT_FILES", "CUTLASS_LOG_FILE", "CUTLASS_WORKSPACE_ROOT",
		"CUTLASS_RETRY_MAX_ATTEMPTS", "CUTLASS_DISABLE_OPENROUTER_MODELS", "CUTLASS_DISABLE_PROMPT_CACHE",
		"CUTLASS_IMAGE_OUTPUT", "CUTLASS_IMAGE_MODEL",
		"CUTLASS_TEMPERATURE", "CUTLASS_MAX_COST_USD", "CUTLASS_MAX_TOTAL_TOKENS",
		// Process/runtime inputs still read after broker boot. A connector cannot
		// claim one of these because removing it would mutate parent behavior.
		"HOME", "PATH", "TMPDIR", "SHELL", "USER", "LANG", "TZ", "NOTIFY_SOCKET",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "SCHED_DATABASE_URL",
		"LLM_MAX_TOKENS",
		"REASONING_ENABLED", "REASONING_EFFORT", "PERSONA", "PERSONA_DEFAULT", "SYSTEM_PROMPT",
		"MAX_ITERATIONS", "LOG_LEVEL", "DEBUG", "VERBOSE",
	}
	if bundle != nil {
		names = append(names, bundle.WebhookSecretEnvNames()...)
		for _, provider := range bundle.Providers {
			names = append(names, provider.APIKeyEnv)
		}
	}
	return names
}

func scrubParentConnectorState(bundle *clientconfig.Bundle, cfg *config.Config, resolvedSpecs map[string]agent.MCPServerSpec) error {
	reloadExcluded := append(bundle.ConnectorEnvVarNames(), bundle.ConnectorAccountEnvVarNames()...)
	if err := cfg.ExcludeEnvVarsFromReload(reloadExcluded...); err != nil {
		return fmt.Errorf("exclude connector environment from parent reload: %w", err)
	}
	keys := bundle.ConnectorEnvironmentKeys(os.Environ())
	var errs []error
	for _, key := range keys {
		if err := os.Unsetenv(key); err != nil {
			errs = append(errs, fmt.Errorf("unset connector environment %s: %w", key, err))
		}
	}
	for name := range resolvedSpecs {
		resolvedSpecs[name] = agent.MCPServerSpec{}
		delete(resolvedSpecs, name)
	}
	for name := range cfg.MCPServers {
		cfg.MCPServers[name] = config.MCPServerConfig{}
		delete(cfg.MCPServers, name)
	}
	for i := range cfg.HTTPTools {
		cfg.HTTPTools[i] = config.HTTPToolConfig{}
	}
	cfg.MCPServers = nil
	cfg.HTTPTools = nil
	bundle.ScrubConnectorRuntimeDefinitions()
	return errors.Join(errs...)
}
