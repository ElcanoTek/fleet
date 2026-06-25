package creds

import (
	"reflect"
	"sort"
	"testing"
)

// TestApplyClientSuffix_EmptyAccountFreshCopy asserts requirement (1): an
// empty account returns a fresh copy of the input env with override count 0,
// and the copy is independent of the caller's map.
func TestApplyClientSuffix_EmptyAccountFreshCopy(t *testing.T) {
	env := map[string]string{"OPENX_API_KEY": "default-key", "OPENX_SEAT": "elcano"}

	out, overrides := ApplyClientSuffix(env, "")
	if overrides != 0 {
		t.Fatalf("empty account override count = %d, want 0", overrides)
	}
	if !reflect.DeepEqual(out, env) {
		t.Fatalf("empty account result = %v, want a copy of %v", out, env)
	}

	// Mutating the returned map must not affect the caller's input.
	out["OPENX_API_KEY"] = "mutated"
	if env["OPENX_API_KEY"] != "default-key" {
		t.Fatalf("returned map aliases the caller's input: input mutated to %q", env["OPENX_API_KEY"])
	}
}

// TestApplyClientSuffix_OverridesBareVar asserts requirement (2): a set
// <VAR>_<ACCOUNT> overrides the bare var and increments the override count.
func TestApplyClientSuffix_OverridesBareVar(t *testing.T) {
	t.Setenv("OPENX_API_KEY_REKLAIM", "reklaim-key")
	t.Setenv("OPENX_SEAT_REKLAIM", "reklaim")

	env := map[string]string{"OPENX_API_KEY": "default-key", "OPENX_SEAT": "elcano"}

	out, overrides := ApplyClientSuffix(env, "reklaim")
	if overrides != 2 {
		t.Fatalf("override count = %d, want 2", overrides)
	}
	if out["OPENX_API_KEY"] != "reklaim-key" {
		t.Errorf("OPENX_API_KEY = %q, want %q", out["OPENX_API_KEY"], "reklaim-key")
	}
	if out["OPENX_SEAT"] != "reklaim" {
		t.Errorf("OPENX_SEAT = %q, want %q", out["OPENX_SEAT"], "reklaim")
	}
}

// TestApplyClientSuffix_NeverAddsVarsAbsentFromInput asserts requirement (3):
// a var NOT in the input env is never added even when its suffixed form is set
// in the process environment.
func TestApplyClientSuffix_NeverAddsVarsAbsentFromInput(t *testing.T) {
	// Suffixed form of a var the caller did NOT pass in the base env.
	t.Setenv("UNRELATED_SECRET_REKLAIM", "should-not-leak")
	t.Setenv("OPENX_API_KEY_REKLAIM", "reklaim-key")

	env := map[string]string{"OPENX_API_KEY": "default-key"}

	out, overrides := ApplyClientSuffix(env, "reklaim")
	if overrides != 1 {
		t.Fatalf("override count = %d, want 1", overrides)
	}
	if _, present := out["UNRELATED_SECRET"]; present {
		t.Error("UNRELATED_SECRET leaked into the result though it was not in the input env")
	}
	if len(out) != 1 {
		t.Errorf("result has %d keys, want exactly 1 (the input var)", len(out))
	}
}

// TestApplyClientSuffix_EmptySuffixedFallsBack asserts requirement (4): an
// empty suffixed value is treated as unset and falls back to the bare value
// (so it does not count as an override).
func TestApplyClientSuffix_EmptySuffixedFallsBack(t *testing.T) {
	t.Setenv("OPENX_API_KEY_REKLAIM", "") // explicitly empty
	t.Setenv("OPENX_SEAT_REKLAIM", "reklaim")

	env := map[string]string{"OPENX_API_KEY": "default-key", "OPENX_SEAT": "elcano"}

	out, overrides := ApplyClientSuffix(env, "reklaim")
	if overrides != 1 {
		t.Fatalf("override count = %d, want 1 (only the non-empty suffixed var counts)", overrides)
	}
	if out["OPENX_API_KEY"] != "default-key" {
		t.Errorf("OPENX_API_KEY = %q, want fallback %q", out["OPENX_API_KEY"], "default-key")
	}
	if out["OPENX_SEAT"] != "reklaim" {
		t.Errorf("OPENX_SEAT = %q, want %q", out["OPENX_SEAT"], "reklaim")
	}
}

// TestApplyClientSuffix_StripsQuotes verifies that a quoted suffixed value is
// injected unquoted, matching the .env.local loading semantics.
func TestApplyClientSuffix_StripsQuotes(t *testing.T) {
	t.Setenv("OPENX_API_KEY_REKLAIM", `"quoted-key"`)

	env := map[string]string{"OPENX_API_KEY": "default-key"}
	out, overrides := ApplyClientSuffix(env, "reklaim")
	if overrides != 1 {
		t.Fatalf("override count = %d, want 1", overrides)
	}
	if out["OPENX_API_KEY"] != "quoted-key" {
		t.Errorf("OPENX_API_KEY = %q, want unquoted %q", out["OPENX_API_KEY"], "quoted-key")
	}
}

// TestApplyClientSuffix_NoSuffixedVarsRefusalGuard documents the overrides==0
// refusal signal that callers (the MCP loader) rely on: requesting a named
// account when no suffixed vars exist for the server returns override count 0,
// so the caller can refuse to spawn a variant that would carry the default
// account's credentials under an account label. The refusal itself is
// implemented by callers, not by this package.
func TestApplyClientSuffix_NoSuffixedVarsRefusalGuard(t *testing.T) {
	env := map[string]string{"OPENX_API_KEY": "default-key", "OPENX_SEAT": "elcano"}

	out, overrides := ApplyClientSuffix(env, "someaccount")
	if overrides != 0 {
		t.Fatalf("override count = %d, want 0 so callers can refuse the variant", overrides)
	}
	// With no suffixed vars set, the variant env is byte-for-byte the base.
	if !reflect.DeepEqual(out, env) {
		t.Fatalf("result = %v, want identical to base env %v (the silent-mixup case callers must refuse)", out, env)
	}
}

// TestAccountsFor asserts requirement (5): AccountsFor lists exactly the
// accounts with non-empty suffixed values, lowercased and distinct, and
// ignores empty suffixed values and vars not in the base set.
func TestAccountsFor(t *testing.T) {
	// Two base vars; reklaim has both, client_a has one, ghost has only an
	// empty value, and a non-base var's suffix must be ignored.
	t.Setenv("OPENX_API_KEY_REKLAIM", "k1")
	t.Setenv("OPENX_SEAT_REKLAIM", "s1")     // same account, second var
	t.Setenv("OPENX_API_KEY_CLIENT_A", "k2") // distinct account
	t.Setenv("OPENX_API_KEY_GHOST", "")      // empty → not an account
	t.Setenv("UNRELATED_SECRET_OTHER", "x")  // not a base var → ignored

	got := AccountsFor([]string{"OPENX_API_KEY", "OPENX_SEAT"})
	sort.Strings(got)

	want := []string{"client_a", "reklaim"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccountsFor = %v, want %v", got, want)
	}
}

// TestAccountsFor_NoAccounts verifies AccountsFor returns no accounts when no
// suffixed vars are set for any base var.
func TestAccountsFor_NoAccounts(t *testing.T) {
	if got := AccountsFor([]string{"OPENX_API_KEY"}); len(got) != 0 {
		t.Fatalf("AccountsFor with no suffixed vars = %v, want empty", got)
	}
}

// TestCanonicalAccount verifies separator folding (hyphen, space → underscore)
// without case change, idempotency, and surrounding-whitespace trim.
func TestCanonicalAccount(t *testing.T) {
	cases := []struct{ in, want string }{
		{"client-a", "client_a"},
		{"client a", "client_a"},
		{"client_a", "client_a"}, // already canonical → no-op
		{"Client-A", "Client_A"}, // case is preserved
		{"  client-a  ", "client_a"},
		{"a-b c", "a_b_c"},
		{"", ""},
	}
	for _, c := range cases {
		if got := CanonicalAccount(c.in); got != c.want {
			t.Errorf("CanonicalAccount(%q) = %q, want %q", c.in, got, c.want)
		}
		// Idempotent: folding the result again changes nothing.
		if again := CanonicalAccount(CanonicalAccount(c.in)); again != c.want {
			t.Errorf("CanonicalAccount not idempotent for %q: %q", c.in, again)
		}
	}
}

// TestAccountsFor_FoldsSeparators is the #146 acceptance test: a hyphen-style
// and an underscore-style suffix for the SAME account collapse to one seat
// rather than forking two.
func TestAccountsFor_FoldsSeparators(t *testing.T) {
	t.Setenv("OPENX_API_KEY_CLIENT_A", "k1")
	t.Setenv("OPENX_API_KEY_CLIENT-A", "k2") // hyphen spelling of the same seat

	got := AccountsFor([]string{"OPENX_API_KEY"})
	want := []string{"client_a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccountsFor with `_CLIENT_A` and `_CLIENT-A` = %v, want one account %v", got, want)
	}
}

// TestApplyClientSuffix_FoldsSeparatorAccount proves the hyphen spelling of an
// account resolves to the underscore-suffixed seat at consume time.
func TestApplyClientSuffix_FoldsSeparatorAccount(t *testing.T) {
	t.Setenv("OPENX_API_KEY_CLIENT_A", "client-a-key")
	base := map[string]string{"OPENX_API_KEY": "default-key"}

	out, overrides := ApplyClientSuffix(base, "client-a") // hyphen spelling
	if overrides != 1 {
		t.Fatalf("overrides = %d, want 1 (hyphen account must resolve to the _CLIENT_A seat)", overrides)
	}
	if out["OPENX_API_KEY"] != "client-a-key" {
		t.Errorf("OPENX_API_KEY = %q, want %q", out["OPENX_API_KEY"], "client-a-key")
	}
}

// TestAccountsFor_NeverLeaksSecretValues is a credential-hygiene guard (#159
// Track 3): the account CATALOG derived from os.Environ must surface account
// NAMES only, never the secret VALUES. A regression that returned values (or
// serialized the environ) would put connector secrets in the web UI's
// account picker and audit log — the "credentials never enter the model
// context / logs" invariant. (ApplyClientSuffix legitimately carries the value
// in the spawned subprocess env; that is the one place a value belongs.)
func TestAccountsFor_NeverLeaksSecretValues(t *testing.T) {
	const secret = "sk-superSecretValue-DO-NOT-LEAK"
	t.Setenv("PROVIDER_API_KEY", secret)
	t.Setenv("PROVIDER_API_KEY_ACME", secret+"-acme")
	t.Setenv("PROVIDER_API_KEY_BETACORP", secret+"-beta")

	accounts := AccountsFor([]string{"PROVIDER_API_KEY"})
	for _, a := range accounts {
		if a == "" {
			continue
		}
		// Account names are derived suffixes (e.g. "acme"), never the value.
		if contains(a, secret) || a == secret {
			t.Fatalf("AccountsFor leaked a secret value in an account name: %q", a)
		}
	}
	// The catalog is the suffixes, lowercased/canonicalized — confirm shape.
	got := append([]string(nil), accounts...)
	sort.Strings(got)
	want := []string{"acme", "betacorp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AccountsFor = %v, want %v", got, want)
	}
}

// contains reports whether sub is a substring of s (avoids importing strings
// just for the guard above).
func contains(s, sub string) bool {
	if sub == "" {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
