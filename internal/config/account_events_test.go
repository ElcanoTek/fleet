package config

import (
	"strings"
	"testing"
)

func TestValidateAccountEvents(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"off", Config{}, ""},
		{"url and secret", Config{AccountEventsURL: "https://auth.example.com/apps/fleet/events", AccountEventsSecret: "s"}, ""},
		{"loopback receiver", Config{AccountEventsURL: "http://127.0.0.1:9000/apps/fleet/events", AccountEventsSecret: "s"}, ""},
		{"url without secret", Config{AccountEventsURL: "https://auth.example.com/e"}, "FLEET_ACCOUNT_EVENTS_SECRET"},
		{"relative url", Config{AccountEventsURL: "/events", AccountEventsSecret: "s"}, "absolute http(s)"},
		{"other scheme", Config{AccountEventsURL: "ftp://x/e", AccountEventsSecret: "s"}, "absolute http(s)"},
		{"secret shared with the task webhook", Config{AccountEventsURL: "https://auth.example.com/e", AccountEventsSecret: "webhook-key"}, "must differ from FLEET_WEBHOOK_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateAccountEvents("webhook-key")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadRefusesUnsignedAccountEvents: the check runs in Load, the path both
// `fleet serve` and `fleet validate-config` take.
func TestLoadRefusesUnsignedAccountEvents(t *testing.T) {
	t.Setenv("FLEET_ACCOUNT_EVENTS_URL", "https://auth.example.com/apps/fleet/events")
	t.Setenv("FLEET_ACCOUNT_EVENTS_SECRET", "")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "FLEET_ACCOUNT_EVENTS_SECRET") {
		t.Fatalf("Load = %v, want the missing-secret refusal", err)
	}
	t.Setenv("FLEET_ACCOUNT_EVENTS_SECRET", "shared")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with both set: %v", err)
	}
	if cfg.AccountEventsURL == "" || cfg.AccountEventsSecret != "shared" {
		t.Fatalf("cfg = %q / %q", cfg.AccountEventsURL, cfg.AccountEventsSecret)
	}
}

// TestAccountEventsSecretKeepsItsBytes: the HMAC key is used exactly as
// loaded, like FLEET_WEBHOOK_SECRET; only emptiness is judged trimmed.
func TestAccountEventsSecretKeepsItsBytes(t *testing.T) {
	t.Setenv("FLEET_ACCOUNT_EVENTS_URL", "https://auth.example.com/apps/fleet/events")
	t.Setenv("FLEET_ACCOUNT_EVENTS_SECRET", " key ")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AccountEventsSecret != " key " {
		t.Fatalf("secret = %q, want it byte for byte", cfg.AccountEventsSecret)
	}
	if err := (&Config{AccountEventsURL: "https://a.example.com/e", AccountEventsSecret: "   "}).validateAccountEvents(""); err == nil {
		t.Fatal("an all-whitespace secret was accepted")
	}
}
