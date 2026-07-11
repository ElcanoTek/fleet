package agentcore

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/mcp"
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
	// Dir is the cwd the stdio subprocess launches in (the client-config bundle
	// root) so relative args like `mcp/foo.py` resolve there; "" inherits the
	// fleet process cwd.
	Dir string
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
		// Inherited-routing-identity guard: a partially-suffixed account (API
		// key overridden, owner/member id not) must be refused, not spawned —
		// it would transact in the DEFAULT seat's identity under the named
		// account's label. Mirrors the cutlass mcp_loader guard.
		suffix := "_" + upperAccount(account)
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
	var registered []string
	for _, choice := range selection {
		base, ok := bases[choice.Server]
		if !ok {
			// A server absent from the active catalog is EITHER misspelled OR
			// known-to-the-manifest but gated off because its default-seat
			// credentials are unset (a server provisioned only as a named account
			// — its <VAR>_<ACCOUNT> set but the bare <VAR> empty — is excluded by
			// the enable gate). Surface both so the operator knows to check the
			// default-seat env, not just the spelling.
			return registered, fmt.Errorf(
				"mcp selection references server %q which is not in the active catalog — "+
					"it is either misspelled or configured-but-gated-off (its default-seat "+
					"credentials are unset; every connector needs its bare default-seat env "+
					"set before a named account can be selected)", choice.Server)
		}

		name, variantEnv, err := resolveMCPVariant(choice.Server, base, choice.Account)
		if err != nil {
			return registered, err
		}

		// HTTP servers register via headers (no env overlay, no account variants).
		if base.HTTPURL != "" {
			if err := client.AddHTTPServerWithOptions(ctx, name, base.HTTPURL, mcp.HTTPServerOptions{Headers: base.HTTPHeaders, TLS: base.HTTPTLS}); err != nil {
				if base.Required {
					return registered, fmt.Errorf("register http server %q: %w", name, err)
				}
				log.Printf("mcp: skipping best-effort http server %q — failed to register: %v", name, err)
				continue
			}
			registered = append(registered, name)
			continue
		}

		// NewStdioTransport sets variantEnv on cmd.Env — credentials are never on
		// argv and never enter the sandbox container. base.Dir pins the subprocess
		// cwd to the bundle root so relative `mcp/*.py` args resolve.
		//
		// Graceful degradation (#182): a best-effort server that fails to start
		// (transport error, bad command, init timeout) is logged and SKIPPED so the
		// rest of the selection still registers and the run proceeds with the tools
		// that did come up. A Required server still aborts the run.
		variantEnv = ExpandWorkspaceEnv(variantEnv, workdir)
		if err := client.AddStdioServer(ctx, name, base.Command, base.Args, variantEnv, base.Dir); err != nil {
			if base.Required {
				return registered, fmt.Errorf("register server %q: %w", name, err)
			}
			log.Printf("mcp: skipping best-effort server %q — failed to register: %v", name, err)
			continue
		}
		registered = append(registered, name)
	}
	return registered, nil
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
