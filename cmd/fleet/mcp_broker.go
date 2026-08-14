package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
	"github.com/ElcanoTek/fleet/internal/secretbox"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/google/uuid"
)

const (
	mcpBrokerRemoteDBMaxOpen = 8
	mcpBrokerRemoteDBMaxIdle = 2
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
	var remoteStore *store.Store
	var remoteMCP agent.RemoteMCPResolver
	if mcpBrokerRemoteConfigured() {
		runtimeCfg, loadErr := config.Load("")
		if loadErr != nil {
			return fmt.Errorf("load broker runtime config: %w", loadErr)
		}
		remoteStore, remoteMCP, err = openMCPBrokerRemoteStore(runtimeCfg)
		if err != nil {
			return err
		}
	}

	// The SAME builder the interactive Manager uses — one credential path. Inline
	// http_tools (issue #261) are resolved + registered HERE, in the broker process
	// that holds the connector secrets, so their auth headers never cross back to
	// the parent fleet process (only public tool descriptors + rendered text do).
	specs := scheduledrun.BuildMCPSpecs(cfg)
	client := agent.BuildMCPClient(specs, cfg.HTTPTools)
	log.Printf("mcp-broker: serving %d MCP tools over stdio", len(client.GetAllTools()))

	backend := &brokerBackend{
		MCPBroker:   agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		client:      client,
		bases:       scheduledrun.BuildMCPBases(cfg),
		httpTools:   cfg.HTTPTools,
		bundleAllow: bundleToolAllowlist(specs),
		enabled:     enabledServerSet(specs),
		scopes:      make(map[string]*brokerScope),
		remoteMCP:   remoteMCP,
		remoteStore: remoteStore,
	}
	serveErr := mcpbroker.ServeStdio(context.Background(), backend)
	return errors.Join(serveErr, backend.Close())
}

func mcpBrokerRemoteConfigured() bool {
	configured := func(suffix string) bool {
		for _, prefix := range []string{"FLEET_", "CHAT_", "CUTLASS_"} {
			if strings.TrimSpace(os.Getenv(prefix+suffix)) != "" {
				return true
			}
		}
		return false
	}
	return configured("MCP_OAUTH_ENCRYPTION_KEY") && configured("PUBLIC_BASE_URL")
}

func openMCPBrokerRemoteStore(cfg *config.Config) (*store.Store, agent.RemoteMCPResolver, error) {
	if cfg == nil || len(cfg.MCPOAuthEncryptionKey) == 0 || cfg.PublicBaseURL == "" {
		return nil, nil, nil
	}
	cipher, err := secretbox.NewCipher(cfg.MCPOAuthEncryptionKey)
	if err != nil {
		return nil, nil, fmt.Errorf("configure broker remote MCP cipher: %w", err)
	}
	chatDB := chatDSN(cfg)
	if err := ensureDistinctDatabases(chatDB, schedDSN()); err != nil {
		return nil, nil, err
	}
	maxOpen := min(cfg.ChatDBPool.MaxOpenConns, mcpBrokerRemoteDBMaxOpen)
	if maxOpen <= 0 {
		maxOpen = mcpBrokerRemoteDBMaxOpen
	}
	maxIdle := min(cfg.ChatDBPool.MaxIdleConns, mcpBrokerRemoteDBMaxIdle, maxOpen)
	if maxIdle < 0 {
		maxIdle = 0
	}
	st, err := store.Open(chatDB, store.PoolConfig{
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    maxIdle,
		ConnMaxIdleTime: cfg.ChatDBPool.ConnMaxIdleTime,
		ConnMaxLifetime: cfg.ChatDBPool.ConnMaxLifetime,
		ConnectTimeout:  cfg.ChatDBPool.ConnectTimeout,
	})
	if err != nil {
		// Driver/DSN errors may echo connection details. The broker owns those
		// values, so startup reports only the failed boundary operation.
		return nil, nil, errors.New("open broker remote MCP store")
	}
	st.SetTokenCipher(cipher)
	svc := remotemcp.NewService(st, remotemcp.Config{
		PublicBaseURL:     cfg.PublicBaseURL,
		AllowInsecureHTTP: cfg.RemoteMCPAllowInsecureHTTP,
	})
	if !svc.Enabled() {
		_ = st.Close()
		return nil, nil, errors.New("broker remote MCP service is not enabled")
	}
	return st, svc, nil
}

// loadMCPBrokerConfig resolves the bundle against the credential-owning
// process's boot environment. It is used at boot and catalog reload so resolved
// server definitions never need to cross the broker protocol; credential env
// changes deliberately require a process restart.
func loadMCPBrokerConfig() (*config.Config, error) {
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return nil, fmt.Errorf("load client config bundle: %w", err)
	}
	return &config.Config{
		MCPServers: bundle.MCPServerConfigs(),
		HTTPTools:  bundle.HTTPToolConfigs(),
	}, nil
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
	// bundleAllow / enabled are the child's OWN Gate-2 allowlist and enabled
	// server set, re-derived from the bundle this process loads (and reloads).
	// They are the authoritative floor every scope is built from — the parent
	// can narrow them, never widen them (issue #167 residual 1). Both are
	// guarded by reloadMu together with bases/httpTools.
	bundleAllow map[string][]string
	enabled     map[string]bool

	reloadMu    sync.Mutex
	mu          sync.RWMutex
	scopes      map[string]*brokerScope
	remoteMCP   agent.RemoteMCPResolver
	remoteStore *store.Store
}

type brokerScope struct {
	mu     sync.RWMutex
	client *mcp.Client
	broker agentcore.MCPBroker
	// authz is this scope's effective child-side gate. Calls are checked
	// against it before they reach the credentialed client, and the scope's
	// advertised catalog was already filtered through it at open.
	authz *scopeAuthorizer
}

// bundleToolAllowlist projects the child's own resolved specs onto the Gate-2
// shape: bundle server name → allowed tool names. Only enabled servers appear;
// a server with no manifest tool_allowlist is absent, which means "every tool
// it exports" exactly as in-process.
func bundleToolAllowlist(specs map[string]agent.MCPServerSpec) map[string][]string {
	allow := make(map[string][]string, len(specs))
	for name, spec := range specs {
		if spec.Enabled && len(spec.ToolAllowlist) > 0 {
			allow[name] = append([]string(nil), spec.ToolAllowlist...)
		}
	}
	return allow
}

// enabledServerSet is the child's own view of which bundle servers exist and
// are on. A scope selection naming anything else is refused, not bound.
func enabledServerSet(specs map[string]agent.MCPServerSpec) map[string]bool {
	out := make(map[string]bool, len(specs))
	for name, spec := range specs {
		if spec.Enabled {
			out[name] = true
		}
	}
	return out
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

func (b *brokerBackend) OpenScope(ctx context.Context, spec mcpbroker.ScopeSpec) (string, []mcpbroker.ToolDescriptor, []string, error) {
	if spec.Remote != nil {
		return b.openRemoteScope(ctx, *spec.Remote, spec.Policy)
	}
	selection := make(agentcore.MCPSelection, 0, len(spec.Selection))
	for _, choice := range spec.Selection {
		selection = append(selection, agentcore.MCPChoice{Server: choice.Server, Account: choice.Account})
	}
	// A scope opening concurrently with reload gets one coherent snapshot of the
	// bases AND the gates derived from the same bundle read: whichever operation
	// acquires reloadMu first. Active scopes remain unchanged.
	b.reloadMu.Lock()
	bases := cloneMCPBases(b.bases)
	httpTools := append([]config.HTTPToolConfig(nil), b.httpTools...)
	bundleAllow := cloneToolAllowlist(b.bundleAllow)
	enabled := cloneServerSet(b.enabled)
	b.reloadMu.Unlock()
	// Authorize the selection against the child's OWN enabled set before
	// anything is spawned. The parent's claim about which servers exist is not
	// evidence (issue #167 residual 1).
	if err := validateScopeSelection(selection, enabled); err != nil {
		return "", nil, nil, err
	}
	for name, base := range bases {
		base.BaseEnv = agentcore.ExpandTaskIDEnv(base.BaseEnv, spec.TaskID)
		bases[name] = base
	}

	client := mcp.NewClient()
	if _, err := agentcore.BindMCPSelection(ctx, client, selection, bases, spec.Workspace); err != nil {
		_ = client.Close()
		return "", nil, nil, err
	}
	var extraServers []string
	if len(httpTools) > 0 {
		agent.RegisterHTTPTools(client, httpTools)
		// Inline http_tools register onto a synthetic server with no manifest
		// entry, so there is no bundle allowlist for it; it is admitted
		// explicitly rather than falling through the "unknown server" refusal.
		extraServers = append(extraServers, clientconfig.HTTPToolServerName)
	}

	authz := newScopeAuthorizer(selection, bundleAllow, spec.Policy, extraServers...)
	scope := &brokerScope{
		client: client,
		// Gate-3 wraps the same helper the in-process loop uses, so a per-task
		// credential allowlist is enforced identically on both sides of the
		// boundary rather than by a second implementation.
		broker: agentcore.GateMCPBrokerWithAllowlist(
			agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
			scopeCredentialAllowlist(spec.Policy),
		),
		authz: authz,
	}
	id := uuid.NewString()
	b.mu.Lock()
	b.scopes[id] = scope
	b.mu.Unlock()
	return id, authz.filterTools(describeTools(client)), nil, nil
}

func (b *brokerBackend) openRemoteScope(ctx context.Context, spec mcpbroker.RemoteScopeSpec, policy *mcpbroker.ScopePolicy) (string, []mcpbroker.ToolDescriptor, []string, error) {
	if b.remoteMCP == nil {
		return "", nil, nil, errors.New("remote MCP OAuth is not configured in broker")
	}
	shadowed := make(map[string]bool, len(spec.Shadowed))
	for _, name := range spec.Shadowed {
		shadowed[name] = true
	}
	var enabled map[string]bool
	if spec.FilterEnabled {
		enabled = make(map[string]bool, len(spec.Enabled))
		for _, name := range spec.Enabled {
			enabled[name] = true
		}
	}
	overlay, err := agent.BuildRemoteMCPOverlay(ctx, b.remoteMCP, spec.UserEmail, shadowed, enabled)
	if err != nil {
		// Resolver errors can contain database/provider detail. Discard the value;
		// the parent receives only a stable, credential-free failure.
		return "", nil, nil, errors.New("remote MCP scope unavailable")
	}
	client := mcp.NewClient()
	var skipped []string
	if overlay != nil {
		if overlay.Client != nil {
			_ = client.Close()
			client = overlay.Client
		}
		skipped = append([]string(nil), overlay.Skipped...)
	}
	tools := describeTools(client)
	connected := make([]string, 0, len(tools))
	for _, tool := range tools {
		if !slices.Contains(connected, tool.Server) {
			connected = append(connected, tool.Server)
		}
	}
	// The scope is bound to exactly the remote servers the child's own resolver
	// connected — never to a name the parent merely asked for.
	authz := newRemoteScopeAuthorizer(connected, policy)
	scope := &brokerScope{
		client: client,
		broker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		authz:  authz,
	}
	id := uuid.NewString()
	b.mu.Lock()
	b.scopes[id] = scope
	b.mu.Unlock()
	return id, authz.filterTools(tools), skipped, nil
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
	specs := scheduledrun.BuildMCPSpecs(cfg)
	summary, err := b.client.Reload(ctx, agent.MCPServerDefs(specs))
	if err != nil {
		return nil, err
	}
	b.bases = scheduledrun.BuildMCPBases(cfg)
	// Re-derive the child's authoritative gates from the SAME bundle read that
	// produced the new bases, so a reload can never leave the enforcement floor
	// describing servers that no longer exist (issue #167 residual 1).
	b.bundleAllow = bundleToolAllowlist(specs)
	b.enabled = enabledServerSet(specs)
	accounts := make(map[string][]string, len(specs))
	servers := make([]mcpbroker.ServerDescriptor, 0, len(specs))
	for name, spec := range specs {
		if spec.Enabled {
			accounts[name] = creds.AccountsFor(spec.AccountVars)
			servers = append(servers, mcpbroker.ServerDescriptor{
				Name:             name,
				ToolAllowlist:    append([]string(nil), spec.ToolAllowlist...),
				AccountVars:      append([]string(nil), spec.AccountVars...),
				Optional:         spec.Optional,
				DisplayName:      spec.DisplayName,
				Description:      spec.Description,
				Beta:             spec.Beta,
				EnabledByDefault: spec.EnabledByDefault,
				DataSources:      append([]string(nil), spec.DataSources...),
				UsesWorkspace:    agentcore.EnvReferencesWorkspace(spec.Env),
			})
		}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })

	return &mcpbroker.ReloadResult{
		Summary: mcpbroker.ReloadSummary{
			Added:     append([]string(nil), summary.Added...),
			Removed:   append([]string(nil), summary.Removed...),
			Restarted: append([]string(nil), summary.Restarted...),
			Unchanged: append([]string(nil), summary.Unchanged...),
		},
		Tools:    describeTools(b.client),
		Accounts: accounts,
		Servers:  servers,
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
	// Authorize before the credentialed client sees the call. The scope's
	// catalog was filtered at open, so a parent asking for a denied tool here is
	// asking for something it was never advertised (issue #167 residual 1).
	if !scope.authz.permits(server, tool) {
		return denyPolicy(server, tool), true, nil
	}
	return scope.broker.CallMCP(ctx, server, tool, args)
}

// CallMCP is the UNSCOPED shared-client path. Production agent turns and
// scheduled runs never reach it — both fail closed without a scope — and
// approval execution reopens the seat it staged under, so what remains is
// legacy approval rows that recorded no seat, plus operator tooling. It is
// nonetheless gated with the child's own bundle allowlist rather than left
// wide open: the shared client carries every enabled server on its default
// seat, which is exactly the reach this boundary exists to bound.
func (b *brokerBackend) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	b.reloadMu.Lock()
	allowed := b.bundleAllow[server]
	enabled := b.enabled[server]
	b.reloadMu.Unlock()
	// The synthetic inline-http-tools server has no manifest entry to gate on.
	if !enabled && server != clientconfig.HTTPToolServerName {
		return denyPolicy(server, tool), true, nil
	}
	if len(allowed) > 0 && !slices.Contains(allowed, tool) {
		return denyPolicy(server, tool), true, nil
	}
	return b.MCPBroker.CallMCP(ctx, server, tool, args)
}

func cloneToolAllowlist(src map[string][]string) map[string][]string {
	dst := make(map[string][]string, len(src))
	for name, list := range src {
		dst[name] = append([]string(nil), list...)
	}
	return dst
}

func cloneServerSet(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for name, on := range src {
		dst[name] = on
	}
	return dst
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
	if b.remoteStore != nil {
		errs = append(errs, b.remoteStore.Close())
	}
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
