// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWebEnvMigrationCarriesEveryKey — a box upgraded from the Next.js tier
// (ADR-0091) keeps sign-in working only if every setting its fleet-web.env
// held reaches fleet.env. The fixture is every key that file ever carried
// (main's web/src process.env reads plus what bootstrap wrote); the
// migration is scripts/lib/web-tier.sh's, run with update.sh's real
// upsert_env_file, i.e. exactly what update.sh and bootstrap.sh execute.
func TestWebEnvMigrationCarriesEveryKey(t *testing.T) {
	root := repoRootFromTest(t)
	update, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	upsert := regexp.MustCompile(`(?ms)^upsert_env_file\(\) \{.*?^\}`).Find(update)
	if upsert == nil {
		t.Fatal("update.sh has no upsert_env_file")
	}

	dir := t.TempDir()
	oldEnv := filepath.Join(dir, "fleet-web.env")
	newEnv := filepath.Join(dir, "fleet.env")
	old := map[string]string{
		"APP_SESSION_SECRET":            "s3cret",
		"AUTH_SIGNING_PUBKEY":           "pub",
		"AUTH_SIGNING_PREVIOUS_PUBKEYS": "old1,old2",
		"AUTH_COOKIE_NAME":              "elcano_auth",
		"AUTH_COOKIE_DOMAIN":            "example.com",
		"AUTH_LOGIN_URL":                "https://auth.example.com",
		"CATWALK_URL":                   "https://catwalk.example.com",
		"FLEET_OIDC_ISSUER":             "https://idp.example.com",
		"FLEET_OIDC_CLIENT_ID":          "fleet",
		"FLEET_OIDC_CLIENT_SECRET":      "oidc-secret",
		"FLEET_OIDC_SCOPES":             "openid email profile",
		"FLEET_OIDC_ALLOWED_DOMAINS":    "example.com",
		"FLEET_OIDC_BUTTON_LABEL":       "Sign in with Example",
		"FLEET_OIDC_REDIRECT_URI":       "https://fleet.example.com/api/auth/oidc/callback",
		"FLEET_OIDC_AUTO_START":         "1",
		"NEXT_PUBLIC_PUBLIC_ORIGIN":     "https://fleet.example.com",
		"FLEET_WEB_HOST":                "0.0.0.0",
		"PORT":                          "3000",
		// Plumbing of the retired node tier: deliberately NOT carried.
		"CHAT_SERVER_URL":           "http://127.0.0.1:8080",
		"CHAT_SERVER_TOKEN":         "tok",
		"ORCHESTRATOR_SERVER_URL":   "http://127.0.0.1:8000",
		"ORCHESTRATOR_SERVER_TOKEN": "wrong-admin-key",
		"NODE_ENV":                  "production",
		"FLEET_NODE_BIN":            "/usr/bin/node-24",
		"NEXT_PUBLIC_APP_NAME":      "Fleet",
		"NEXT_PUBLIC_BUILD_ID":      "abc",
	}
	var b strings.Builder
	for k, v := range old {
		b.WriteString(k + "=" + v + "\n")
	}
	if err := os.WriteFile(oldEnv, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// An operator value already in fleet.env always wins.
	if err := os.WriteFile(newEnv, []byte("AUTH_COOKIE_DOMAIN=operator.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := "ok() { :; }; warn() { echo \"WARN $*\"; }; info() { :; }\n" + string(upsert) + "\n" +
		". " + filepath.Join(root, "scripts", "lib", "web-tier.sh") + "\n" +
		`fleet_migrate_web_env "$1" "$2"` + "\n"
	for run := 0; run < 2; run++ { // a re-run is a no-op
		out, err := exec.Command("bash", "-c", script, "migrate", oldEnv, newEnv).CombinedOutput()
		if err != nil || strings.Contains(string(out), "WARN") {
			t.Fatalf("migration run %d: %v\n%s", run, err, out)
		}
	}

	got := map[string]string{}
	raw, err := os.ReadFile(newEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		if _, dup := got[k]; dup {
			t.Errorf("%s written twice", k)
		}
		got[k] = v
	}
	want := map[string]string{
		"FLEET_PUBLIC_BASE_URL": "https://fleet.example.com",
		"FLEET_WEB_ADDR":        "0.0.0.0:3000",
		"AUTH_COOKIE_DOMAIN":    "operator.example",
	}
	for _, k := range []string{
		"APP_SESSION_SECRET", "AUTH_SIGNING_PUBKEY", "AUTH_SIGNING_PREVIOUS_PUBKEYS", "AUTH_COOKIE_NAME",
		"AUTH_LOGIN_URL", "CATWALK_URL", "FLEET_OIDC_ISSUER", "FLEET_OIDC_CLIENT_ID", "FLEET_OIDC_CLIENT_SECRET",
		"FLEET_OIDC_SCOPES", "FLEET_OIDC_ALLOWED_DOMAINS", "FLEET_OIDC_BUTTON_LABEL", "FLEET_OIDC_REDIRECT_URI",
		"FLEET_OIDC_AUTO_START",
	} {
		want[k] = old[k]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("fleet.env holds %d keys, want exactly %d (node-tier plumbing must not be carried):\n%s", len(got), len(want), raw)
	}

	// Both entry points use the ONE list — no private copy that could drift.
	for _, s := range []string{"update.sh", "bootstrap.sh"} {
		body, err := os.ReadFile(filepath.Join(root, "scripts", s))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "fleet_migrate_web_env ") {
			t.Errorf("%s does not call fleet_migrate_web_env", s)
		}
		if strings.Contains(string(body), "FLEET_OIDC_CLIENT_SECRET") || strings.Contains(string(body), "APP_SESSION_SECRET|AUTH_SIGNING_PUBKEY") {
			t.Errorf("%s keeps its own copy of the carried-key list", s)
		}
	}
}
