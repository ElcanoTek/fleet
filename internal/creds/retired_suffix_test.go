package creds

import "testing"

// ADR-0106 (Codex P2 on #2081): an account-suffixed removed spelling is never
// read as an override, while a live suffixed variant still is.
func TestApplyClientSuffixIgnoresRetiredSuffixedNames(t *testing.T) {
	t.Setenv("CHAT_SERVER_TOKEN_ACME", "stale")
	t.Setenv("PROVIDER_KEY_ACME", "live-acme")
	out, n := ApplyClientSuffix(map[string]string{"CHAT_SERVER_TOKEN": "base", "PROVIDER_KEY": "base-key"}, "acme")
	if out["CHAT_SERVER_TOKEN"] != "base" {
		t.Errorf("CHAT_SERVER_TOKEN = %q; the removed CHAT_SERVER_TOKEN_ACME must not be read", out["CHAT_SERVER_TOKEN"])
	}
	if out["PROVIDER_KEY"] != "live-acme" || n != 1 {
		t.Errorf("PROVIDER_KEY = %q (overrides %d); the live suffixed variant must still apply", out["PROVIDER_KEY"], n)
	}
}
