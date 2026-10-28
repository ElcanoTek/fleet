// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package config

import (
	"path/filepath"
	"testing"
)

// PasswordLoginDisabled follows central Auth SSO being configured, by the same
// rule the browser tier uses to offer SSO, unless the break-glass is on.
func TestLoad_PasswordLoginDisabledFollowsCentralAuth(t *testing.T) {
	sso := map[string]string{
		"FLEET_OIDC_ISSUER":        "https://auth.example.com",
		"FLEET_OIDC_CLIENT_ID":     "fleet",
		"FLEET_OIDC_CLIENT_SECRET": "s3cret",
	}
	with := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range sso {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"no SSO", nil, false},
		{"SSO configured", sso, true},
		{"break-glass on", with(map[string]string{"FLEET_OIDC_ALLOW_PASSWORD": "1"}), false},
		{"break-glass spelled true", with(map[string]string{"FLEET_OIDC_ALLOW_PASSWORD": "true"}), false},
		{"break-glass off", with(map[string]string{"FLEET_OIDC_ALLOW_PASSWORD": "0"}), true},
		{"issuer only slashes", with(map[string]string{"FLEET_OIDC_ISSUER": " / "}), false},
		{"no client id", with(map[string]string{"FLEET_OIDC_CLIENT_ID": ""}), false},
		{"blank secret", with(map[string]string{"FLEET_OIDC_CLIENT_SECRET": "  "}), false},
		{"break-glass without SSO", map[string]string{"FLEET_OIDC_ALLOW_PASSWORD": "1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			chdir(t, t.TempDir())
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PasswordLoginDisabled != tc.want {
				t.Errorf("PasswordLoginDisabled = %v, want %v", cfg.PasswordLoginDisabled, tc.want)
			}
			// Auth manages accounts whenever SSO is configured; the
			// break-glass reopens passwords, not account management.
			if cfg.AccountsManagedByAuth != OIDCConfigured() {
				t.Errorf("AccountsManagedByAuth = %v with SSO configured = %v", cfg.AccountsManagedByAuth, OIDCConfigured())
			}
		})
	}
}

// The decision is bound at boot, so a reload never applies it but reports any
// edit that would change it: turning the break-glass on, or configuring Auth
// SSO on a box that had none. An unrelated edit reports nothing.
func TestReload_PasswordDecisionChangeReportedSkipped(t *testing.T) {
	const sso = "FLEET_OIDC_ISSUER=https://auth.example.com\nFLEET_OIDC_CLIENT_ID=fleet\nFLEET_OIDC_CLIENT_SECRET=s\n"
	const key = "FLEET_OIDC_* / FLEET_OIDC_ALLOW_PASSWORD"
	skipped := func(res ReloadResult) bool {
		for _, s := range res.Skipped {
			if s.Key == key {
				return true
			}
		}
		return false
	}
	cases := []struct {
		name, boot, edit string
		want             bool
	}{
		{"break-glass turned on", sso, sso + "FLEET_OIDC_ALLOW_PASSWORD=1\n", true},
		{"SSO configured", "", sso, true},
		{"unrelated edit", sso, sso + "FLEET_MAX_COST_USD=7\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			envPath := filepath.Join(t.TempDir(), ".env")
			writeEnv(t, envPath, tc.boot)
			cfg, err := Load(envPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			before := cfg.PasswordLoginDisabled
			writeEnv(t, envPath, tc.edit)
			res, err := cfg.Reload(envPath)
			if err != nil {
				t.Fatalf("Reload: %v", err)
			}
			if got := skipped(res); got != tc.want {
				t.Errorf("reported %v, want %v: %+v", got, tc.want, res.Skipped)
			}
			if cfg.PasswordLoginDisabled != before {
				t.Error("a reload changed PasswordLoginDisabled")
			}
		})
	}
}

// One way (ADR-0095): on a Fleet whose accounts Auth manages, an account feed
// still pointed at that Auth is switched off; one pointed anywhere else, or on
// a Fleet that manages its own accounts, is kept.
func TestLoad_AccountFeedToAuthIsIgnoredWhenAuthManagesAccounts(t *testing.T) {
	sso := map[string]string{
		"FLEET_OIDC_ISSUER":        "https://auth.example.com",
		"FLEET_OIDC_CLIENT_ID":     "fleet",
		"FLEET_OIDC_CLIENT_SECRET": "s3cret",
	}
	cases := []struct {
		name, url   string
		sso, ignore bool
	}{
		{"feed at Auth", "https://AUTH.example.com/apps/fleet/events", true, true},
		{"feed elsewhere", "https://audit.example.com/hook", true, false},
		{"no SSO", "https://auth.example.com/apps/fleet/events", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			chdir(t, t.TempDir())
			if tc.sso {
				for k, v := range sso {
					t.Setenv(k, v)
				}
			}
			t.Setenv("FLEET_ACCOUNT_EVENTS_URL", tc.url)
			t.Setenv("FLEET_ACCOUNT_EVENTS_SECRET", "feed-secret-0123456789abcdef0123456789")
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AccountEventsFeedToAuthIgnored != tc.ignore || (cfg.AccountEventsURL == "") != tc.ignore {
				t.Errorf("ignored=%v url=%q secret set=%v", cfg.AccountEventsFeedToAuthIgnored, cfg.AccountEventsURL, cfg.AccountEventsSecret != "")
			}
			if tc.ignore && cfg.AccountEventsSecret != "" {
				t.Error("the feed's secret was kept")
			}
		})
	}
}
