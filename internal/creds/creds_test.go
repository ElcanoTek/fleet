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
