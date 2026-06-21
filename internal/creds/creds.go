// Package creds is the MCP credential-account store for the fleet monorepo.
//
// Secrets live at rest in the 0600 env file (matching how chat and moc store
// recoverable secrets today). A credential "account" is expressed as an env
// suffix: the bare var <VAR> backs the default/shared seat, and <VAR>_<ACCOUNT>
// backs a named account. This package provides the two pure operations the
// rest of fleet needs over that convention:
//
//   - [ApplyClientSuffix] overlays an account's <VAR>_<ACCOUNT> values over a
//     base env map, returning the per-account env to inject into an MCP
//     subprocess plus the count of vars that were actually overridden. Callers
//     use the override count to refuse spawning a named-account variant that
//     would silently inherit the default seat's credentials.
//   - [AccountsFor] derives the account catalog by scanning the process
//     environment for <VAR>_<SUFFIX> keys with non-empty values.
//
// Credentials produced here are injected into MCP subprocesses host-side via
// cmd.Env only; they never reach the sandbox container and never appear on
// argv. See docs/MIGRATION_PLAN_V2.md §6.
package creds

import (
	"os"
	"strings"
)

// ApplyClientSuffix returns a copy of env with each var overridden by its
// `_<UPPER(account)>` variant when that variant is set in the process
// environment. The second return value is the count of vars that were
// actually overridden — callers use it to detect "account requested but no
// suffixed vars are set" and fail loudly rather than silently shipping
// bare credentials into the wrong account.
//
// When account is empty, env is returned as-is (a fresh copy so callers
// may mutate without affecting the caller's map) and the override count
// is 0.
//
// Vars not in the input env are NEVER added even when their suffixed form
// is set in the process environment — this prevents leaking unrelated env
// state into a spawned subprocess.
//
// Convention: OPENX_API_KEY (default / Elcano) + OPENX_API_KEY_REKLAIM
// (Reklaim's account). Adding a new account requires only setting their
// suffixed env vars — no code change. Empty suffixed values are treated
// the same as unset and fall back to the bare value (matches the
// `getEnvOrDefault` semantics used elsewhere).
func ApplyClientSuffix(env map[string]string, account string) (map[string]string, int) {
	out := make(map[string]string, len(env))
	if account == "" {
		for k, v := range env {
			out[k] = v
		}
		return out, 0
	}
	suffix := "_" + strings.ToUpper(account)
	overrides := 0
	for k, v := range env {
		if override := os.Getenv(k + suffix); override != "" {
			out[k] = stripQuotes(override)
			overrides++
			continue
		}
		out[k] = v
	}
	return out, overrides
}

// AccountsFor scans the process environment for `<VAR>_<SUFFIX>` keys backing
// any of baseVars and returns the distinct account suffixes (lowercased) that
// have a non-empty value. This is the account catalog derived by suffix scan,
// mirroring chat's collectGammaKeys / cutlass's account-name listing.
//
// A suffix is only reported when at least one of its `<VAR>_<SUFFIX>` keys has
// a non-empty value, so an account whose key was provisioned and later cleared
// does not appear as an available account. Each baseVar is matched
// case-insensitively against the `<VAR>_` prefix; the captured suffix is
// lowercased so `Client_A`, `CLIENT_A`, and `client_a` collapse to one seat.
func AccountsFor(baseVars []string) []string {
	seen := make(map[string]struct{})
	var accounts []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if val == "" {
			continue
		}
		upperKey := strings.ToUpper(key)
		for _, base := range baseVars {
			prefix := strings.ToUpper(base) + "_"
			rest, ok := strings.CutPrefix(upperKey, prefix)
			if !ok || rest == "" {
				continue
			}
			account := strings.ToLower(rest)
			if _, dup := seen[account]; dup {
				break
			}
			seen[account] = struct{}{}
			accounts = append(accounts, account)
			break
		}
	}
	return accounts
}

// stripQuotes removes a single layer of matching surrounding single or double
// quotes from an env value, matching the env-file loading semantics used
// across fleet so a quoted `.env.local` value is injected unquoted.
func stripQuotes(value string) string {
	if (strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) ||
		(strings.HasPrefix(value, `'`) && strings.HasSuffix(value, `'`)) {
		if len(value) >= 2 {
			return value[1 : len(value)-1]
		}
	}
	return value
}
