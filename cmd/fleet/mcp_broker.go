package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/ms365mcp"
	"github.com/ElcanoTek/fleet/internal/otelsetup"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/version"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
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
// so it never corrupts the frame stream the parent decodes. The child exports its
// spans to the parent's collector as service "<FLEET_SERVICE_NAME>" with
// fleet.process=mcp-broker, so a delegated call's span lands in the trace of
// the turn or run that made it (#2052, internal/mcpbroker/telemetry.go).
//
// mcpBrokerLogConfig is the broker child's logging: always JSON, at the
// parent's level (FLEET_LOG_LEVEL), and NO file of its own. JSON is the wire
// format between the two processes, not the operator's choice: the parent
// (logging.ChildStderr) parses each child record and re-logs it through its
// own configured handler — the operator's FLEET_LOG_FORMAT, its redactor,
// its sinks, FLEET_LOG_FILE under the one rotator the parent owns.
func mcpBrokerLogConfig() logging.Config {
	return logging.Config{
		Format: "json",
		Level:  envreg.Get("FLEET_LOG_LEVEL"),
	}
}

// mcpBrokerServeContext is the broker's serve context: cancelled by SIGTERM,
// so a signalled broker returns from Serve and runs its deferred shutdown
// (backend close, bounded span flush) rather than dying mid-flush.
func mcpBrokerServeContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM)
}

func runMCPBroker() error {
	// Same handler and redactor as the parent, in JSON: the parent parses each
	// line and re-logs it in the operator's format (mcpBrokerLogConfig).
	if _, err := logging.Configure(mcpBrokerLogConfig()); err != nil {
		return fmt.Errorf("configure broker logging: %w", err)
	}
	shutdownTracing, err := otelsetup.InitWith(context.Background(), otelsetup.Options{ServiceVersion: version.String(), Process: "mcp-broker"})
	if err != nil {
		// Tracing is optional observability, exactly as in the parent: a bad
		// exporter configuration disables it rather than the broker.
		slog.Error("mcp-broker: otel init failed, tracing disabled", "err", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		// Bounded below the parent's SIGTERM-to-SIGKILL grace
		// (mcpbroker.SpawnClient), so the flush finishes before a kill.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			slog.Warn("mcp-broker: otel shutdown error", "err", err)
		}
	}()
	// SIGTERM (the parent's teardown, or a supervisor's) ends Serve like EOF
	// does, so the deferred backend close and span flush run instead of the
	// process dying mid-flush.
	serveCtx, stopSignals := mcpBrokerServeContext()
	defer stopSignals()
	cfg, err := loadMCPBrokerConfig()
	if err != nil {
		return err
	}
	var remoteStore *store.Store
	var remoteMCP agent.RemoteMCPResolver
	var runtimeCfg *config.Config
	if mcpBrokerRemoteConfigured() {
		var loadErr error
		runtimeCfg, loadErr = config.Load("")
		if loadErr != nil {
			return fmt.Errorf("load broker runtime config: %w", loadErr)
		}
		remoteStore, remoteMCP, err = openMCPBrokerRemoteStore(runtimeCfg)
		if err != nil {
			return err
		}
	}
	ms365RuntimeCfg := runtimeCfg
	if ms365RuntimeCfg == nil {
		ms365RuntimeCfg = cfg
	}
	ms365Handler, ms365Cfg := wireMS365MCP(ms365RuntimeCfg)
	if ms365Handler != nil {
		slog.Info("mcp-broker: built-in Microsoft 365 MCP enabled",
			"tenant_id", safe.LogSafe(ms365Cfg.TenantID),
			"public_url", safe.LogSafe(ms365Cfg.PublicURL),
		)
		closeAttachmentSrv := serveInheritedMS365AttachmentListener(serveCtx, ms365Handler)
		defer func() { _ = closeAttachmentSrv() }()
	}

	// The SAME builder the interactive Manager uses — one credential path. Inline
	// http_tools (issue #261) are resolved + registered HERE, in the broker process
	// that holds the connector secrets, so their auth headers never cross back to
	// the parent fleet process (only public tool descriptors + rendered text do).
	specs := agent.BuildMCPSpecs(cfg)
	client := agent.BuildMCPClient(specs, cfg.HTTPTools, cfg.A2APeers)
	slog.Info("mcp-broker: serving MCP tools over stdio", "tools", len(client.GetAllTools()))

	backend := &brokerBackend{
		MCPBroker:   agentcore.NewLocalMCPBroker(client),
		client:      client,
		bases:       agent.BuildMCPBases(cfg),
		httpTools:   cfg.HTTPTools,
		a2aPeers:    cfg.A2APeers,
		bundleAllow: bundleToolAllowlist(specs),
		enabled:     enabledServerSet(specs),
		scopes:      make(map[string]*brokerScope),
		remoteMCP:   remoteMCP,
		remoteStore: remoteStore,
		ms365:       ms365Handler,
	}
	serveErr := mcpbroker.ServeStdio(serveCtx, backend)
	return errors.Join(serveErr, backend.Close())
}

func mcpBrokerRemoteConfigured() bool {
	configured := func(name string) bool {
		return strings.TrimSpace(envreg.Get(name)) != ""
	}
	return configured("FLEET_MCP_OAUTH_ENCRYPTION_KEY") && configured("FLEET_PUBLIC_BASE_URL")
}

func serveInheritedMS365AttachmentListener(ctx context.Context, h http.Handler) func() error {
	f := os.NewFile(3, "ms365-attachment-listener")
	if f == nil {
		return func() error { return nil }
	}
	ln, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return func() error { return nil }
	}
	srv := &http.Server{
		Handler:           ms365mcp.AttachmentOnlyHandler(h),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		_ = srv.Serve(ln)
	}()
	return srv.Close
}

func openMCPBrokerRemoteStore(cfg *config.Config) (*store.Store, agent.RemoteMCPResolver, error) {
	if cfg == nil || len(cfg.MCPOAuthEncryptionKey) == 0 || cfg.PublicBaseURL == "" {
		return nil, nil, nil
	}
	cipher, err := secretbox.NewCipher(cfg.MCPOAuthEncryptionKey)
	if err != nil {
		return nil, nil, fmt.Errorf("configure broker remote MCP cipher: %w", err)
	}
	chatDB := cfg.DatabaseURL
	// cfg.DatabaseURL is config.ResolveDSNs' chat DSN, so it already carries
	// the chat schema; the broker agrees with `fleet serve` without deciding
	// anything itself. The distinct-stores check is a cheap backstop for a
	// Config assembled some other way.
	if err := config.EnsureDistinctStores(chatDB, cfg.SchedDatabaseURL); err != nil {
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
	// Runtime-acquired credentials — per-user OAuth bearers, rotated refresh
	// tokens, unsealed api_key secrets — join THIS process's literal redactor at
	// acquisition time (#1124): boot-time RegisterEnvLiterals only knows env-file
	// secrets, so without this a connector echoing its own bare token into an
	// error string would reach the masked-error log unscrubbed. Rotations are
	// scoped per server row (#1274), so this process's literal set plateaus
	// instead of accumulating every token pair it has ever refreshed.
	svc := newRemoteMCPService(cfg, st, mcpbroker.RegisterSecretLiterals)
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
	cfg, _, err := loadMCPBrokerBundle()
	return cfg, err
}

// loadMCPBrokerBundle is loadMCPBrokerConfig plus the bundle's public
// connector metadata from the same read, so a reload's picker view always
// describes the catalog it reloaded.
func loadMCPBrokerBundle() (*config.Config, []mcpbroker.ConnectorDescriptor, error) {
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return nil, nil, fmt.Errorf("load client config bundle: %w", err)
	}
	meta := bundle.ConnectorMeta()
	connectors := make([]mcpbroker.ConnectorDescriptor, 0, len(meta))
	for _, m := range meta {
		connectors = append(connectors, mcpbroker.ConnectorDescriptor{
			Name:        m.Name,
			DisplayName: m.DisplayName,
			Description: m.Description,
			Category:    m.Category,
			Optional:    m.Optional,
			Configured:  m.Configured,
			Tools:       append([]string(nil), m.Tools...),
		})
	}
	return &config.Config{
		MCPServers: bundle.MCPServerConfigs(),
		HTTPTools:  bundle.HTTPToolConfigs(),
		A2APeers:   bundle.A2APeerConfigs(),
	}, connectors, nil
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
	a2aPeers            []config.A2APeerConfig
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
	ms365       *ms365mcp.Handler
}

type brokerScope struct {
	mu     sync.RWMutex
	client *mcp.Client
	broker agentcore.MCPBroker
	// remote marks a per-user hosted scope: a failed call's answer from one
	// of ITS servers is a vendor's own reply and may cross the pipe bounded
	// and scrubbed (mcpbroker.HostedCallError, ADR-0075); a bundle scope's
	// errors stay masked.
	remote bool
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

func (b *brokerBackend) OpenScope(ctx context.Context, spec mcpbroker.ScopeSpec) (string, []mcpbroker.ToolDescriptor, []mcpbroker.SkippedServer, error) {
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
	a2aPeers := append([]config.A2APeerConfig(nil), b.a2aPeers...)
	bundleAllow := cloneToolAllowlist(b.bundleAllow)
	enabled := maps.Clone(b.enabled)
	b.reloadMu.Unlock()
	// Authorize the selection against the child's OWN enabled set before
	// anything is spawned. The parent's claim about which servers exist is not
	// evidence (issue #167 residual 1).
	if err := validateScopeSelection(selection, enabled); err != nil {
		return "", nil, nil, err
	}
	// The interactive turn's signed-in user (ADR-0087). Re-validated here —
	// the credential owner does not trust the parent's formatting — and a
	// value that fails is treated as absent, which drops the keys that need
	// it. No user (every scheduled scope; validateScopeSpec refuses a task
	// scope that carries one) drops every ${FLEET_USER_EMAIL}-bearing key.
	var user agentcore.MCPUserIdentity
	if spec.User != nil {
		user = agentcore.NewMCPUserIdentity(spec.User.Email)
	}
	for name, base := range bases {
		// Stamped, not substituted: the binder writes the task id and the user
		// into the env at spawn (agentcore.SpawnEnv), after the account
		// overlay and its identity guard, in the one shared token order.
		base.TaskID = spec.TaskID
		base.User = user
		bases[name] = base
	}

	client := mcp.NewClient()
	_, failed, err := agentcore.BindMCPSelectionReport(ctx, client, selection, bases, spec.Workspace)
	if err != nil {
		_ = client.Close()
		return "", nil, nil, err
	}
	// Failed registrations cross as public names plus a credential-free
	// detail, so the parent's requirements check can tell a connector outage
	// from a server that was never configured.
	var skipped []mcpbroker.SkippedServer
	for _, f := range failed {
		skipped = append(skipped, mcpbroker.SkippedServer{Name: f.Server, Reason: agent.SkipReasonUnreachable, Detail: f.Detail, Transient: f.Transient})
	}
	var extraServers []string
	if len(httpTools) > 0 {
		agent.RegisterHTTPTools(client, httpTools)
		// Inline http_tools register onto a synthetic server with no manifest
		// entry, so there is no bundle allowlist for it; it is admitted
		// explicitly rather than falling through the "unknown server" refusal.
		extraServers = append(extraServers, clientconfig.HTTPToolServerName)
	}
	if len(a2aPeers) > 0 {
		// Outbound A2A peers (#1368) ride the same synthetic-server seam. The
		// scope's default delegation depth comes from the parent's policy —
		// the task row's inbound depth — because a call context cannot cross
		// the stdio boundary; 0 (no policy) is human-initiated work.
		depth := 0
		if spec.Policy != nil {
			depth = spec.Policy.A2ADepth
		}
		agent.RegisterA2APeers(client, a2aPeers, depth)
		extraServers = append(extraServers, clientconfig.A2AToolServerName)
	}

	authz := newScopeAuthorizer(selection, bundleAllow, spec.Policy, extraServers...)
	scope := &brokerScope{
		client: client,
		// Gate-3 wraps the same helper the in-process loop uses, so a per-task
		// credential allowlist is enforced identically on both sides of the
		// boundary rather than by a second implementation.
		broker: agentcore.GateMCPBrokerWithAllowlist(
			agentcore.NewLocalMCPBroker(client),
			scopeCredentialAllowlist(spec.Policy),
		),
		authz: authz,
	}
	id := uuid.NewString()
	b.mu.Lock()
	b.scopes[id] = scope
	b.mu.Unlock()
	return id, authz.filterTools(describeTools(client)), skipped, nil
}

func (b *brokerBackend) openRemoteScope(ctx context.Context, spec mcpbroker.RemoteScopeSpec, policy *mcpbroker.ScopePolicy) (string, []mcpbroker.ToolDescriptor, []mcpbroker.SkippedServer, error) {
	if b.remoteMCP == nil {
		return "", nil, nil, errors.New("remote MCP OAuth is not configured in broker")
	}
	shadowed := make(map[string]bool, len(spec.Shadowed))
	for _, name := range spec.Shadowed {
		shadowed[name] = true
	}
	sel := agent.RemoteMCPSelection{Filter: spec.FilterEnabled, Exact: spec.Exact}
	if spec.FilterEnabled {
		sel.Enabled = make(map[string]bool, len(spec.Enabled))
		for _, name := range spec.Enabled {
			sel.Enabled[name] = true
		}
	}
	if len(spec.Accounts) > 0 {
		sel.Accounts = make(map[string]string, len(spec.Accounts))
		for name, account := range spec.Accounts {
			sel.Accounts[name] = account
		}
	}
	overlay, err := agent.BuildRemoteMCPOverlay(ctx, b.remoteMCP, spec.UserEmail, shadowed, sel)
	if err != nil {
		// Resolver errors can contain database/provider detail. Discard the value;
		// the parent receives only a stable, credential-free failure.
		return "", nil, nil, errors.New("remote MCP scope unavailable")
	}
	client := mcp.NewClient()
	var skipped []mcpbroker.SkippedServer
	if overlay != nil {
		if overlay.Client != nil {
			_ = client.Close()
			client = overlay.Client
		}
		// The name and its reason class cross the wire; the failure detail
		// (which can quote a resolved URL or a vendor body) stays here.
		failures := make(map[string]agentcore.MCPConnectFailure, len(overlay.ConnectFailures))
		for _, f := range overlay.ConnectFailures {
			failures[f.Server] = f
		}
		for _, name := range overlay.Skipped {
			seat := overlay.SkippedSeats[name]
			f := failures[name]
			skipped = append(skipped, mcpbroker.SkippedServer{Name: name, Reason: overlay.SkipReason(name), Server: seat.Server, Account: seat.Account, Detail: f.Detail, Transient: f.Transient})
		}
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
		broker: agentcore.NewLocalMCPBroker(client),
		authz:  authz,
		remote: true,
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

	var (
		cfg        *config.Config
		connectors []mcpbroker.ConnectorDescriptor
		err        error
	)
	if b.reloadConfig != nil {
		cfg, err = b.reloadConfig()
	} else {
		cfg, connectors, err = loadMCPBrokerBundle()
	}
	if err != nil {
		return nil, err
	}
	if b.ms365 != nil {
		if err := b.ms365.ReloadCatalogFromEnv(); err != nil {
			return nil, fmt.Errorf("reload ms365 catalog: %w", err)
		}
	}
	specs := agent.BuildMCPSpecs(cfg)
	// A reloaded catalog may carry new header credentials: the redactors
	// learn them before Reload contacts any server.
	agent.RegisterConnectorSecrets(specs, cfg.HTTPTools, cfg.A2APeers)
	summary, err := b.client.Reload(ctx, agent.MCPServerDefs(specs))
	if err != nil {
		return nil, err
	}
	b.bases = agent.BuildMCPBases(cfg)
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
		Tools:      describeTools(b.client),
		Accounts:   accounts,
		Servers:    servers,
		Connectors: connectors,
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
	text, isErr, err := scope.broker.CallMCP(ctx, server, tool, args)
	if err != nil && scope.remote {
		// A hosted vendor's own answer may cross the pipe bounded and
		// scrubbed (mcpbroker.describeCallError, ADR-0075); the marker is what
		// lets it. Bundle scopes leave their errors unmarked, so they stay
		// masked.
		err = &mcpbroker.HostedCallError{Err: err}
	}
	return text, isErr, err
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
	// The synthetic inline-http-tools and a2a-peer servers have no manifest
	// entry to gate on.
	if !enabled && server != clientconfig.HTTPToolServerName && server != clientconfig.A2AToolServerName {
		return denyPolicy(server, tool), true, nil
	}
	if len(allowed) > 0 && !slices.Contains(allowed, tool) {
		return denyPolicy(server, tool), true, nil
	}
	return b.MCPBroker.CallMCP(ctx, server, tool, args)
}

// cloneToolAllowlist deep-copies a server → names map (the tool allowlist and
// the account inventory share the shape): a fresh map and fresh slices.
func cloneToolAllowlist(src map[string][]string) map[string][]string {
	dst := make(map[string][]string, len(src))
	for name, list := range src {
		dst[name] = append([]string(nil), list...)
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
		base.BaseEnv = maps.Clone(base.BaseEnv)
		base.HTTPHeaders = maps.Clone(base.HTTPHeaders)
		base.Args = append([]string(nil), base.Args...)
		base.IdentityEnv = append([]string(nil), base.IdentityEnv...)
		dst[name] = base
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
			Critical:    st.Tool.Critical,
		})
	}
	return out
}
