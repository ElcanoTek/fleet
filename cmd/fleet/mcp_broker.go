package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
	"github.com/google/uuid"
)

// runMCPBroker is `fleet mcp-broker`: the out-of-process MCP credential broker
// (issue #167). It owns the connector secrets — loaded from the env file into THIS
// process — builds the credentialed MCP client + its server subprocesses, and
// serves CallMCP + discovery over its stdio to the parent fleet process. The
// parent's agent loop then holds no connector secrets: it delegates every MCP call
// here, so "in-process" means only where the loop runs, not where secrets live.
//
// Frames ride stdin/stdout; all logging goes to stderr (the std logger's default)
// so it never corrupts the frame stream the parent decodes.
func runMCPBroker() error {
	cfg, err := loadMCPBrokerConfig()
	if err != nil {
		return err
	}

	// The SAME builder the interactive Manager uses — one credential path. Inline
	// http_tools (issue #261) are resolved + registered HERE, in the broker process
	// that holds the connector secrets, so their auth headers never cross back to
	// the parent fleet process (only public tool descriptors + rendered text do).
	client := agent.BuildMCPClient(scheduledrun.BuildMCPSpecs(cfg), cfg.HTTPTools)
	log.Printf("mcp-broker: serving %d MCP tools over stdio", len(client.GetAllTools()))

	backend := &brokerBackend{
		MCPBroker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		client:    client,
		bases:     scheduledrun.BuildMCPBases(cfg),
		httpTools: cfg.HTTPTools,
		scopes:    make(map[string]*brokerScope),
	}
	serveErr := mcpbroker.ServeStdio(context.Background(), backend)
	return errors.Join(serveErr, backend.Close())
}

// loadMCPBrokerConfig resolves the bundle and connector environment inside the
// credential-owning process. It is used at boot and reload so resolved server
// definitions never need to cross the broker protocol.
func loadMCPBrokerConfig() (*config.Config, error) {
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return nil, fmt.Errorf("load client config bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)

	cfg, err := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	cfg.MCPServers = bundle.MCPServerConfigs()
	cfg.HTTPTools = bundle.HTTPToolConfigs()
	return cfg, nil
}

// brokerBackend is the mcpbroker.Backend the broker process serves: calls run
// through the in-process localMCPBroker over the credentialed client; discovery
// returns the live tool catalog and the provisioned account names — resolved
// against THIS process's environment, where the secrets live. Only public data
// (rendered text, tool descriptors, account names) ever crosses back to the parent.
type brokerBackend struct {
	agentcore.MCPBroker // CallMCP, via localMCPBroker over the credentialed client
	client              *mcp.Client
	bases               map[string]agentcore.MCPServerBase
	httpTools           []config.HTTPToolConfig
	reloadConfig        func() (*config.Config, error)

	reloadMu sync.Mutex
	mu       sync.RWMutex
	scopes   map[string]*brokerScope
}

type brokerScope struct {
	mu     sync.RWMutex
	client *mcp.Client
	broker agentcore.MCPBroker
}

var (
	_ mcpbroker.Backend       = (*brokerBackend)(nil)
	_ mcpbroker.ScopedBackend = (*brokerBackend)(nil)
	_ mcpbroker.ReloadBackend = (*brokerBackend)(nil)
)

func (b *brokerBackend) ListTools(context.Context) ([]mcpbroker.ToolDescriptor, error) {
	return describeTools(b.client), nil
}

func (b *brokerBackend) ListAccounts(_ context.Context, _ string, baseVars []string) ([]string, error) {
	return creds.AccountsFor(baseVars), nil
}

func (b *brokerBackend) OpenScope(ctx context.Context, spec mcpbroker.ScopeSpec) (string, []mcpbroker.ToolDescriptor, error) {
	selection := make(agentcore.MCPSelection, 0, len(spec.Selection))
	for _, choice := range spec.Selection {
		selection = append(selection, agentcore.MCPChoice{Server: choice.Server, Account: choice.Account})
	}
	// A scope opening concurrently with reload gets one coherent catalog snapshot:
	// whichever operation acquires reloadMu first. Active scopes remain unchanged.
	b.reloadMu.Lock()
	bases := cloneMCPBases(b.bases)
	httpTools := append([]config.HTTPToolConfig(nil), b.httpTools...)
	b.reloadMu.Unlock()
	for name, base := range bases {
		base.BaseEnv = agentcore.ExpandTaskIDEnv(base.BaseEnv, spec.TaskID)
		bases[name] = base
	}

	client := mcp.NewClient()
	if _, err := agentcore.BindMCPSelection(ctx, client, selection, bases, spec.Workspace); err != nil {
		_ = client.Close()
		return "", nil, err
	}
	agent.RegisterHTTPTools(client, httpTools)

	scope := &brokerScope{
		client: client,
		broker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
	}
	id := uuid.NewString()
	b.mu.Lock()
	b.scopes[id] = scope
	b.mu.Unlock()
	return id, describeTools(client), nil
}

// Reload re-reads credential-bearing configuration inside the child, applies
// the minimum shared-client diff, and publishes new bases for future scopes.
// Existing scopes deliberately retain the catalog they opened with.
func (b *brokerBackend) Reload(ctx context.Context) (*mcpbroker.ReloadResult, error) {
	b.reloadMu.Lock()
	defer b.reloadMu.Unlock()

	load := b.reloadConfig
	if load == nil {
		load = loadMCPBrokerConfig
	}
	cfg, err := load()
	if err != nil {
		return nil, err
	}
	summary, err := b.client.Reload(ctx, agent.MCPServerDefs(scheduledrun.BuildMCPSpecs(cfg)))
	if err != nil {
		return nil, err
	}
	b.bases = scheduledrun.BuildMCPBases(cfg)

	return &mcpbroker.ReloadResult{
		Summary: mcpbroker.ReloadSummary{
			Added:     append([]string(nil), summary.Added...),
			Removed:   append([]string(nil), summary.Removed...),
			Restarted: append([]string(nil), summary.Restarted...),
			Unchanged: append([]string(nil), summary.Unchanged...),
		},
		Tools: describeTools(b.client),
	}, nil
}

func (b *brokerBackend) CallMCPInScope(ctx context.Context, scopeID, server, tool string, args map[string]any) (string, bool, error) {
	b.mu.RLock()
	scope := b.scopes[scopeID]
	if scope != nil {
		scope.mu.RLock()
	}
	b.mu.RUnlock()
	if scope == nil {
		return "", false, fmt.Errorf("mcpbroker: unknown scope %q", scopeID)
	}
	defer scope.mu.RUnlock()
	return scope.broker.CallMCP(ctx, server, tool, args)
}

func (b *brokerBackend) CloseScope(_ context.Context, scopeID string) error {
	b.mu.Lock()
	scope := b.scopes[scopeID]
	delete(b.scopes, scopeID)
	b.mu.Unlock()
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.client.Close()
}

// Close releases the shared client and every scope left behind by a peer hangup.
func (b *brokerBackend) Close() error {
	b.mu.Lock()
	scopes := b.scopes
	b.scopes = make(map[string]*brokerScope)
	b.mu.Unlock()

	errs := make([]error, 0, len(scopes)+1)
	for _, scope := range scopes {
		scope.mu.Lock()
		errs = append(errs, scope.client.Close())
		scope.mu.Unlock()
	}
	errs = append(errs, b.client.Close())
	return errors.Join(errs...)
}

func cloneMCPBases(src map[string]agentcore.MCPServerBase) map[string]agentcore.MCPServerBase {
	dst := make(map[string]agentcore.MCPServerBase, len(src))
	for name, base := range src {
		base.BaseEnv = cloneStrings(base.BaseEnv)
		base.HTTPHeaders = cloneStrings(base.HTTPHeaders)
		base.Args = append([]string(nil), base.Args...)
		base.IdentityEnv = append([]string(nil), base.IdentityEnv...)
		dst[name] = base
	}
	return dst
}

func cloneStrings(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func describeTools(client *mcp.Client) []mcpbroker.ToolDescriptor {
	tools := client.GetAllTools()
	out := make([]mcpbroker.ToolDescriptor, 0, len(tools))
	for _, st := range tools {
		out = append(out, mcpbroker.ToolDescriptor{
			Server:      st.ServerName,
			Tool:        st.Tool.Name,
			Description: st.Tool.Description,
			InputSchema: st.Tool.InputSchema,
		})
	}
	return out
}
