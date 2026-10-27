package agentcore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/usertoken"
)

// MCPVariantClientEnvVar is injected into every ACCOUNT-VARIANT stdio
// subprocess as the lowercased canonical account name (and never into the
// default seat). It mirrors the cutlass mcp_loader `client=...` convention:
// the cutlass-family Python servers read MCP_VARIANT_CLIENT to (a) require
// variant-scoped identity config instead of silently inheriting the default
// seat's, and (b) derive client-facing labels (e.g. an SSP fee-partner /
// fee-recipient name) — so omitting it would route revenue-bearing fields to
// the DEFAULT client's identity under a named account. Values are data, not
// secrets.
const MCPVariantClientEnvVar = "MCP_VARIANT_CLIENT"

// MCP selection → per-run credentialed wiring (plan §6.1, §6.3).
//
// MCPChoice is which optional server is on + which credential account backs it.
// Account=="" means the default/shared seat. This is chat's opt-in list (a
// []string of server names) with one string added per entry. Both the
// interactive producer (conversation row) and the scheduled producer (task row)
// reduce to an MCPSelection; the SAME binding path turns it into credentialed
// MCP subprocesses.

// MCPChoice names one chosen server and its credential account.
type MCPChoice struct {
	Server  string `json:"server"`            // catalog key, e.g. "myserver"
	Account string `json:"account,omitempty"` // e.g. "client_a"; "" = default
}

// MCPSelection is the per-run list of chosen servers.
type MCPSelection []MCPChoice

// RegisteredMCPName is the name BindMCPSelection registers a {server, account}
// choice under, and therefore the name every dispatch path — the agent loop,
// the credential allowlist projection, and the out-of-process broker — keys on.
// Exported so a caller that must map a registered name back to (or forward
// from) its public selection uses the ONE formula rather than reparsing a name
// whose server part may itself contain underscores.
func RegisteredMCPName(server, account string) string {
	if acct := creds.CanonicalAccount(account); acct != "" {
		return server + "_" + acct
	}
	return server
}

// OptInSet returns the set of enabled server NAMES, derived from the selection.
// This is the per-run enabled set fed to buildFantasyTools' Gate-1 (accounts do
// not affect which tools register).
func (s MCPSelection) OptInSet() map[string]bool {
	out := make(map[string]bool, len(s))
	for _, c := range s {
		if c.Server != "" {
			out[c.Server] = true
		}
	}
	return out
}

// MCPServerBase describes how to spawn one server's stdio subprocess plus the
// base env it expects (before any account overlay). HTTP/fast_io servers set
// HTTPURL instead of Command and reject account variants.
type MCPServerBase struct {
	// BaseEnv is the server's default-seat env (built by the unified
	// ProviderMCPEnv / EmailMCPEnv builders in P3's config package).
	BaseEnv map[string]string
	// Command + Args spawn the stdio subprocess. Empty Command + non-empty
	// HTTPURL means an HTTP server.
	Command string
	Args    []string
	// Dir is the FALLBACK cwd for the stdio subprocess; "" inherits the fleet
	// process cwd. When this spawn has a fleet-managed workspace to offer, the
	// subprocess launches there instead (StdioCwd) so a server's relative
	// output path stops landing in the operator's bundle checkout.
	Dir string
	// DirPinned marks Dir as a contract (an Agent Plugin's plugin root), never
	// overridden by a workspace.
	DirPinned bool
	// HTTPURL, when set, marks this as an HTTP (fast_io) server. HTTP servers
	// reject account variants (credentials are header-based, not env-suffixed).
	HTTPURL string
	// HTTPHeaders are sent with each HTTP request (default seat only).
	HTTPHeaders map[string]string
	// HTTPTLS hardens an HTTP server's connection (CA pinning / mTLS / public-key
	// pin) when set in the manifest (#280); nil = default system TLS.
	HTTPTLS *mcp.TLSOptions
	// Required marks a load-bearing server: if it fails to register, the run
	// aborts. Best-effort servers (the default, Required=false) are skipped with a
	// warning so one flaky server can't kill an otherwise-healthy run (#182).
	Required bool
	// IdentityEnv names the env KEYS (from BaseEnv) that route identity or
	// money — owner/member/account ids, seat-routing tokens (the manifest's
	// per-server identity_env list). A named-account spawn is REFUSED when any
	// of these has a non-empty default-seat value that the account's
	// <VAR>_<ACCOUNT> overlay did NOT override: suffixing the API key but not
	// the owner id would otherwise transact in the DEFAULT client's seat under
	// the named account's label (the cutlass inherited-routing-identity guard).
	IdentityEnv []string
	// User is the signed-in user of the interactive scope this base is bound
	// into (ADR-0087), substituted for ${FLEET_USER_EMAIL} at spawn (SpawnEnv)
	// AFTER the account overlay and its guard. Only the broker child's
	// interactive OpenScope sets it; every other path leaves it zero, which
	// drops the token-bearing keys.
	User MCPUserIdentity
	// TaskID is the scheduled task this base is bound for, substituted for
	// ${FLEET_TASK_ID} at spawn (SpawnEnv). Only the scheduled per-run paths
	// (scheduledrun, the broker child's task scope) set it; every other path
	// leaves it empty, which drops the token-bearing keys. Stamped rather
	// than pre-substituted into BaseEnv so every reserved token is applied
	// by the one helper, in its one order.
	TaskID string
}

// resolveMCPVariant computes the per-run registration name + credentialed env
// for one {server, account} choice WITHOUT spawning anything. This is the pure
// core of the binding (env overlay + the account refusal guard); BindMCPSelection
// calls it before spawning. Tests assert on this helper to verify the overlay
// and refusal semantics without launching MCP subprocesses.
//
//   - account == "" → (server, copy of base env) with no overlay.
//   - named account with overrides → (server_account, <VAR>_<ACCOUNT> overlay).
//   - named account with ZERO overrides → error (the refusal guard).
//   - HTTP server + named account → error (HTTP rejects variants).
func resolveMCPVariant(server string, base MCPServerBase, account string) (name string, env map[string]string, err error) {
	// Canonicalize the account name (separators folded to underscore) so the env
	// overlay, the refusal message, and the registration name all agree with the
	// <VAR>_<UPPER(account)> env key the credential store writes — `client-a` and
	// `client_a` resolve to one seat, never two.
	account = creds.CanonicalAccount(account)
	if base.HTTPURL != "" {
		if account != "" {
			return "", nil, fmt.Errorf("server %q is HTTP and does not support account variants (requested account %q)", server, account)
		}
		return server, nil, nil
	}

	variantEnv, overrides := creds.ApplyClientSuffix(base.BaseEnv, account)
	if account != "" && overrides == 0 {
		return "", nil, fmt.Errorf(
			"refusing to spawn server %q under account %q: no <VAR>_%s credentials are set, so it would silently inherit the default seat",
			server, account, upperAccount(account))
	}

	name = server
	if account != "" {
		// The signed-in-user token is honoured only where the bundle writes it
		// (ADR-0087). A <VAR>_<ACCOUNT> overlay comes from the process env —
		// the credential store, an operator export — so one that names the
		// token would otherwise be filled with whoever is at the keyboard at
		// spawn, an identity_env key included, routing a seat or money by the
		// user. Refuse the seat rather than spawn it.
		suffix := "_" + upperAccount(account)
		for v := range base.BaseEnv {
			if usertoken.Mentions(os.Getenv(v + suffix)) {
				return "", nil, fmt.Errorf(
					"refusing to spawn server %q under account %q: %s%s names the reserved signed-in-user token, which fleet substitutes only where the bundle manifest writes it",
					server, account, v, suffix)
			}
		}
		// Inherited-routing-identity guard: a partially-suffixed account (API
		// key overridden, owner/member id not) must be refused, not spawned —
		// it would transact in the DEFAULT seat's identity under the named
		// account's label. Mirrors the cutlass mcp_loader guard.
		for _, v := range base.IdentityEnv {
			if strings.TrimSpace(base.BaseEnv[v]) == "" {
				continue // identity var unset on the default seat: nothing to inherit
			}
			if strings.TrimSpace(os.Getenv(v+suffix)) != "" {
				continue // overridden for this account
			}
			return "", nil, fmt.Errorf(
				"refusing to spawn server %q under account %q: identity-routing var %s has a default-seat value but no %s%s override — the variant would inherit the default seat's identity",
				server, account, v, v, suffix)
		}
		name = server + "_" + account
		// Tell the server which client variant it is running as (the cutlass
		// mcp_loader convention): lowercased canonical account name, injected
		// only for named-account variants — the default seat never carries it.
		variantEnv[MCPVariantClientEnvVar] = strings.ToLower(account)
	}
	return name, variantEnv, nil
}

// BindMCPSelection converts an MCPSelection into per-run MCP wiring on client,
// the SAME way for both modes. For each chosen {server, account}:
//
//  1. Look up the server's base env + spawn spec via bases[server].
//  2. variantEnv, overrides := creds.ApplyClientSuffix(baseEnv, account) —
//     overlay <VAR>_<ACCOUNT> over <VAR>.
//  3. If account != "" && overrides == 0, REFUSE — never silently inject the
//     default seat under an account label (cutlass guard).
//  4. Register under name `server` (default) or `server_account` (variant) via
//     client.AddStdioServer, which sets variantEnv on cmd.Env (credentials are
//     never on argv and never enter the sandbox). HTTP servers reject variants.
//
// workdir is the writable directory substituted for the reserved
// ${FLEET_WORKSPACE} manifest-env token (see mcp_workspace.go): a per-run dir
// for a run's dedicated client, the shared per-deployment dir for spawns onto a
// shared client. Empty drops token-bearing env keys (fail-safe unset). Catalogs
// that never use the token are unaffected regardless of the value.
//
// Returns the list of registered server names (the keys the agent dispatches
// against) so the caller can scope per-run cleanup.
func BindMCPSelection(ctx context.Context, client *mcp.Client, selection MCPSelection, bases map[string]MCPServerBase, workdir string) ([]string, error) {
	registered, _, err := BindMCPSelectionReport(ctx, client, selection, bases, workdir)
	return registered, err
}

// MCPConnectFailure is one selected server that failed to register for this
// run: it was configured and selected, and connecting to it (or starting it)
// failed. Detail is a credential-free summary of why (mcp.ConnectErrorSummary,
// redacted), safe to cross the broker boundary and to land in a task's error
// message; Transient is mcp.IsTransientConnectError on the last attempt's
// error — weather a later attempt can clear, not a credential or
// configuration problem.
//
// A scheduled run's EXECUTION REQUIREMENTS check reads these: a declared
// server or tool that is absent only because its server failed to connect
// TRANSIENTLY is a connector outage the scheduler re-runs later
// (ErrConnectorUnavailable), not a terminal roster error.
type MCPConnectFailure struct {
	Server    string
	Detail    string
	Transient bool
}

// ErrConnectorUnavailable classifies a scheduled run that could not start
// because every requirement its EXECUTION REQUIREMENTS declare that the roster
// lacks belongs to a server that failed to connect transiently in this run (a
// DNS blip, a vendor answering "temporarily unavailable"). The runner treats
// it as infrastructure weather: it re-runs the same occurrence a few minutes
// later without spending the task's own max_retries, and a dead-letter it
// ends in does not count toward the recurrence park breaker — until three in
// a row, which park the chain on their own (ADR-0077). A server that is
// not configured or not selected, a tool name that does not exist, or a
// connect failure that is not transient (a refused credential) stays the
// terminal roster error it always was.
var ErrConnectorUnavailable = errors.New("required connector unavailable")

// ConnectorUnavailableError is the dispatch error ErrConnectorUnavailable
// classifies: Message is the full roster error (the missing requirements and
// each failed server with its connect error), Failures the transient connect
// failures that explain the miss. The runner records Connectors() on the
// dead-letter so the recurrence breaker can name the connector when it parks
// a chain the outage has kept dead-lettering.
type ConnectorUnavailableError struct {
	Message  string
	Failures []MCPConnectFailure
}

func (e *ConnectorUnavailableError) Error() string { return e.Message }

// Unwrap classifies the error as ErrConnectorUnavailable.
func (e *ConnectorUnavailableError) Unwrap() error { return ErrConnectorUnavailable }

// Connectors renders the failed connectors with their causes, e.g.
// "pages (DNS lookup failed (no such host))", joined by "; ".
func (e *ConnectorUnavailableError) Connectors() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		detail := f.Detail
		if detail == "" {
			detail = "failed to connect"
		}
		parts = append(parts, f.Server+" ("+detail+")")
	}
	return strings.Join(parts, "; ")
}

// NewMCPConnectFailure describes a failed registration of server.
func NewMCPConnectFailure(server string, err error) MCPConnectFailure {
	return MCPConnectFailure{
		Server:    server,
		Detail:    RedactSecrets(mcp.ConnectErrorSummary(err)),
		Transient: mcp.IsTransientConnectError(err),
	}
}

// UnknownMCPServerError is the binder's refusal for a selection entry naming a
// server that is not in the active catalog. A server absent from the active
// catalog is EITHER misspelled OR known-to-the-manifest but gated off because
// its default-seat credentials are unset (a server provisioned only as a named
// account — its <VAR>_<ACCOUNT> set but the bare <VAR> empty — is excluded by
// the enable gate). Surface both so the operator knows to check the
// default-seat env, not just the spelling. Exported so the scheduler's
// create-time selection check refuses with the exact words a run would.
func UnknownMCPServerError(server string) error {
	return fmt.Errorf(
		"mcp selection references server %q which is not in the active catalog — "+
			"it is either misspelled or configured-but-gated-off (its default-seat "+
			"credentials are unset; every connector needs its bare default-seat env "+
			"set before a named account can be selected)", server)
}

// BindMCPSelectionReport is BindMCPSelection that also reports every
// best-effort server it skipped because registration failed. Under a ctx
// marked mcp.WithConnectRetry (scheduled runs), an HTTP server's registration
// is retried while it fails fast and transiently (mcp.RetryTransientConnect:
// up to three attempts, a few seconds apart, within the run's 15 s retry
// budget; a timeout is not retried) before it is skipped — production lost
// whole runs to a one-minute DNS blip and to a vendor's "temporarily
// unavailable" handshake reply. A stdio server is started once, as before.
//
// Binding runs in two phases. The whole selection is resolved first, so a
// configuration error (an unknown server, a refused account variant) aborts
// before anything is spawned. The servers then connect concurrently, at most
// mcpBindConcurrency at a time: every interactive turn opens a fresh scope,
// and connecting one server after another made the turn wait for the SUM of
// their start-up times instead of roughly the slowest one. Results are
// reported in selection order regardless of which server finished first, and
// the error returned for a Required failure is the first one that happened.
// A run that retries connects (WithConnectRetry) still binds sequentially, so
// its shared retry allowance is charged as before. Repeating one choice binds
// it once; two different choices that resolve to the same registration name
// are refused.
func BindMCPSelectionReport(ctx context.Context, client *mcp.Client, selection MCPSelection, bases map[string]MCPServerBase, workdir string) (registered []string, failed []MCPConnectFailure, err error) {
	plans := make([]mcpBindPlan, 0, len(selection))
	byName := make(map[string]MCPChoice, len(selection))
	for _, choice := range selection {
		base, ok := bases[choice.Server]
		if !ok {
			return nil, nil, UnknownMCPServerError(choice.Server)
		}

		name, variantEnv, err := resolveMCPVariant(choice.Server, base, choice.Account)
		if err != nil {
			return nil, nil, err
		}
		// Two different choices can resolve to one registration name ({foo, bar}
		// and {foo_bar, ""} both register as foo_bar). Binding both would race
		// two servers onto one client entry; refuse rather than pick one, since
		// the scope's authorization is keyed by the choice, not the name.
		if prev, dup := byName[name]; dup {
			if creds.CanonicalAccount(prev.Account) == creds.CanonicalAccount(choice.Account) && prev.Server == choice.Server {
				continue
			}
			return nil, nil, fmt.Errorf("mcp selection entries {%s, %q} and {%s, %q} both register as %q",
				prev.Server, prev.Account, choice.Server, choice.Account, name)
		}
		byName[name] = choice
		plans = append(plans, mcpBindPlan{name: name, base: base, env: variantEnv})
	}

	// A Required failure cancels the connects still in flight; the run is
	// aborting, so finishing them only delays the error. Every worker is still
	// joined before returning, so no registration lands after the caller has
	// seen the result (and closed the client).
	bindCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Each slot starts as a failure, so a worker that panics (safe.Go recovers
	// it) can never be reported as registered.
	outcomes := make([]error, len(plans))
	for i := range outcomes {
		outcomes[i] = errMCPBindIncomplete
	}
	var (
		requiredMu  sync.Mutex
		requiredErr error
	)
	bind := func(i int) {
		// Deferred so a panicking connect (recovered by safe.Go) still records
		// a Required failure and cancels its siblings at once, instead of
		// surfacing only after the slowest of them finishes.
		defer func() {
			if outcomes[i] == nil || !plans[i].base.Required {
				return
			}
			requiredMu.Lock()
			// The first Required failure is the cause; a later one may be only
			// the cancellation it triggered.
			if requiredErr == nil {
				requiredErr = outcomes[i]
				if errors.Is(requiredErr, errMCPBindIncomplete) {
					requiredErr = fmt.Errorf("register server %q: %w", plans[i].name, requiredErr)
				}
				cancel()
			}
			requiredMu.Unlock()
		}()
		// A worker that waited for a slot re-checks: a Required failure or the
		// caller ending the run while it queued means nothing more is spawned.
		if err := bindCtx.Err(); err != nil {
			outcomes[i] = fmt.Errorf("register server %q: %w", plans[i].name, err)
			return
		}
		outcomes[i] = bindMCPServer(bindCtx, client, plans[i], workdir)
	}
	if mcp.ConnectRetryBudgetLeft(ctx) > 0 {
		// A run that retries connects (scheduled runs) binds one server at a
		// time, as it always has: the retry allowance is charged per pause, so
		// concurrent retries would each be billed for the same wall-clock wait
		// and starve one another. Only interactive turns, which never retry,
		// take the concurrent path.
		for i := range plans {
			bind(i)
		}
	} else {
		var wg sync.WaitGroup
		sem := make(chan struct{}, mcpBindConcurrency)
		for i := range plans {
			wg.Add(1)
			safe.Go("mcp.bind."+plans[i].name, func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-bindCtx.Done():
					outcomes[i] = bindCtx.Err()
					return
				}
				defer func() { <-sem }()
				bind(i)
			})
		}
		wg.Wait()
	}

	for i, plan := range plans {
		if outcomes[i] == nil {
			registered = append(registered, plan.name)
			continue
		}
		if plan.base.Required {
			// A Required server that never reported (its worker panicked, or
			// the caller's ctx ended before it started) still fails the bind.
			if requiredErr == nil {
				requiredErr = fmt.Errorf("register server %q: %w", plan.name, outcomes[i])
			}
			continue
		}
		log.Printf("mcp: skipping best-effort server %q — failed to register: %v", plan.name, outcomes[i])
		failed = append(failed, NewMCPConnectFailure(plan.name, outcomes[i]))
	}
	if requiredErr != nil {
		return registered, failed, requiredErr
	}
	return registered, failed, nil
}

// errMCPBindIncomplete marks a server whose connect never reported back.
var errMCPBindIncomplete = errors.New("mcp: server connect did not complete")

// mcpBindConcurrency caps how many servers one selection connects at once, so
// a large selection does not fork its whole catalog in a single burst.
const mcpBindConcurrency = 8

// mcpBindPlan is one resolved selection entry: the name it registers under and
// the default-seat or account-variant env it starts with.
type mcpBindPlan struct {
	name string
	base MCPServerBase
	env  map[string]string
}

// bindMCPServer connects one resolved server onto client. Errors carry the
// server name; a Required server's error aborts the whole bind.
func bindMCPServer(ctx context.Context, client *mcp.Client, plan mcpBindPlan, workdir string) error {
	name, base := plan.name, plan.base
	// HTTP servers register via headers (no env overlay, no account variants).
	if base.HTTPURL != "" {
		if err := mcp.RetryTransientConnect(ctx, name, func(attemptCtx context.Context) error {
			return client.AddHTTPServerWithOptions(attemptCtx, name, base.HTTPURL, mcp.HTTPServerOptions{Headers: base.HTTPHeaders, TLS: base.HTTPTLS})
		}); err != nil {
			return fmt.Errorf("register http server %q: %w", name, err)
		}
		return nil
	}

	// NewStdioTransport sets the env on cmd.Env — credentials are never on
	// argv and never enter the sandbox container. The subprocess launches in
	// this scope's workspace when it has one (StdioCwd), else base.Dir;
	// script args are absolutized at bundle load, so neither choice affects
	// whether the server starts.
	//
	// Graceful degradation (#182): a best-effort server that fails to start
	// (transport error, bad command, init timeout) is logged and SKIPPED by the
	// caller so the rest of the selection still registers and the run proceeds
	// with the tools that did come up. A Required server still aborts the run.
	env := SpawnEnv(plan.env, workdir, base.TaskID, base.User)
	cwd := StdioCwd(base.Dir, base.DirPinned, workdir)
	if err := client.AddStdioServer(ctx, name, base.Command, base.Args, env, cwd); err != nil {
		return fmt.Errorf("register server %q: %w", name, err)
	}
	return nil
}

// SpawnEnv applies fleet's reserved spawn-time tokens to a server's resolved
// env in the one safe order. It is the ONLY place they are applied — the
// per-run and broker-scope binder (after resolveMCPVariant's account overlay
// and identity guard), the boot-time shared spawn, hot reload's diff and
// `fleet mcp test` all call it, so a reload compares exactly what the first
// spawn produced:
//
//  1. ${FLEET_USER_EMAIL} FIRST (ADR-0087), on the values the bundle wrote:
//     the signed-in user an interactive broker scope stamped on the base
//     (cmd/fleet OpenScope), so a named seat gets the user exactly like the
//     default seat. Every other caller — scheduled runs, load-on-demand onto
//     a shared client — passes the zero user, which drops the keys. Running
//     it before the path substitutions below means a workspace path that
//     happens to contain the token text can never turn a key the bundle did
//     not write the token into (an identity_env key included) into a
//     user-dependent one; such text stays literal. The email itself can
//     never contain '$', '{' or '}', so it cannot form a later token.
//  2. ${FLEET_WORKSPACE}: workdir (empty drops the keys).
//  3. ${FLEET_TASK_ID}: taskID for a scheduled run's own bases; every other
//     caller passes "" (no task identity), which drops the keys.
//  4. ${FLEET_WORKSPACE_ROOT}: the deployment root, offered on every path.
func SpawnEnv(env map[string]string, workdir, taskID string, user MCPUserIdentity) map[string]string {
	env = ExpandUserEnv(env, user)
	env = ExpandWorkspaceEnv(env, workdir)
	env = ExpandTaskIDEnv(env, taskID)
	return ExpandWorkspaceRootEnv(env)
}

func upperAccount(account string) string {
	out := make([]byte, 0, len(account))
	for i := 0; i < len(account); i++ {
		c := account[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
