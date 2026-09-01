// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/boxdoctor"
)

// renderCaddyfile sources scripts/lib/caddyfile.sh and runs one of its
// functions, returning stdout. Skips when bash is unavailable.
func renderCaddyfile(t *testing.T, fn string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping Caddyfile renderer test")
	}
	root := repoRootFromTest(t)
	lib := filepath.Join(root, "scripts", "lib", "caddyfile.sh")
	script := ". " + lib + "; " + fn + ` "$@"`
	cmd := exec.Command("bash", append([]string{"-c", script, "caddyfile"}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n--- output ---\n%s", fn, args, err, out)
	}
	return string(out)
}

// functionalLines strips comments and blank lines — the same rule
// caddyfile_functional_body / unit_functional_body apply, so the parity test
// below compares what Caddy executes, not the prose around it.
func functionalLines(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

// TestCaddyfileRendererMatchesDeployReference — deploy/Caddyfile is the
// annotated reference operators read; scripts/lib/caddyfile.sh is what
// bootstrap writes, update adopts and doctor repairs against. They drifted once
// (the printf only knew the web tier, so /v1 404'd at Next on every
// bootstrapped box) — this pins them to the same functional body.
func TestCaddyfileRendererMatchesDeployReference(t *testing.T) {
	root := repoRootFromTest(t)
	ref, err := os.ReadFile(filepath.Join(root, "deploy", "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderCaddyfile(t, "render_fleet_caddyfile", "fleet.example.com")
	if got, want := functionalLines(rendered), functionalLines(string(ref)); got != want {
		t.Errorf("scripts/lib/caddyfile.sh and deploy/Caddyfile differ functionally — edit them together.\n--- rendered ---\n%s\n--- deploy/Caddyfile ---\n%s", got, want)
	}
}

// TestCaddyfileRendererRoutesTheAPI — the load-bearing lines: the public API
// paths go to the orchestrator, chat's signed webhooks to the chat listener,
// the Next-proxy header-trust channel is stripped on both (ADR-0053), the SSE
// settings survive on the API route, and the web tier stays the fallback.
func TestCaddyfileRendererRoutesTheAPI(t *testing.T) {
	out := renderCaddyfile(t, "render_fleet_caddyfile", "fleet.example.com")
	if !strings.HasPrefix(out, boxdoctor.CaddyMarker+"\n") {
		t.Errorf("rendered file must start with the marker line; got %q", strings.SplitN(out, "\n", 2)[0])
	}
	if strings.Contains(out, "\temail ") {
		t.Errorf("no ACME email given, but a global email block was rendered:\n%s", out)
	}
	for _, want := range []string{
		"fleet.example.com {",
		"@fleet_api path /v1 /v1/* /api-info /.well-known/agent-card.json /a2a /a2a/* /triggers/*",
		"handle @fleet_api {\n\t\treverse_proxy 127.0.0.1:8000 {",
		"header_up -X-Orchestrator-Server-Token",
		"header_up -X-User-Email",
		"header_up -X-User-Session-Epoch",
		"@fleet_chat_webhooks path /webhooks/*",
		"handle @fleet_chat_webhooks {\n\t\treverse_proxy 127.0.0.1:8080 {",
		"header_up -X-Chat-Server-Token",
		"handle {\n\t\treverse_proxy 127.0.0.1:3000 {",
		"flush_interval -1",
		"read_timeout 30m",
		`Strict-Transport-Security "max-age=63072000; includeSubDomains"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Caddyfile missing %q\n--- output ---\n%s", want, out)
		}
	}
	// Both header-trust headers must be deleted on BOTH backend routes.
	if n := strings.Count(out, "header_up -X-User-Email"); n != 2 {
		t.Errorf("X-User-Email must be stripped on the orchestrator AND chat routes, got %d deletions", n)
	}
	if n := strings.Count(out, "header_up -X-User-Session-Epoch"); n != 2 {
		t.Errorf("X-User-Session-Epoch must be stripped on both routes, got %d deletions", n)
	}
}

// TestCaddyfileRendererRoundTrip — the domain, ACME email and moved listener
// addresses render where expected, and the read-back helpers doctor/update use
// to re-render an installed file recover the domain + email exactly (so a
// repair keeps the operator's Let's Encrypt contact).
func TestCaddyfileRendererRoundTrip(t *testing.T) {
	out := renderCaddyfile(t, "render_fleet_caddyfile", "ops.example.org", "ops@example.org", "127.0.0.1:9080", "127.0.0.1:9000", "127.0.0.1:4000")
	for _, want := range []string{
		"{\n\temail ops@example.org\n}\n",
		"ops.example.org {",
		"reverse_proxy 127.0.0.1:9000 {",
		"reverse_proxy 127.0.0.1:9080 {",
		"reverse_proxy 127.0.0.1:4000 {",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Caddyfile missing %q\n--- output ---\n%s", want, out)
		}
	}
	for _, forbid := range []string{"127.0.0.1:8000", "127.0.0.1:8080", "127.0.0.1:3000"} {
		if strings.Contains(out, forbid) {
			t.Errorf("default upstream %q leaked into a render with overrides", forbid)
		}
	}
	// Empty overrides mean "default", so callers can pass env_get output verbatim.
	def := renderCaddyfile(t, "render_fleet_caddyfile", "fleet.example.com", "", "", "")
	if !strings.Contains(def, "reverse_proxy 127.0.0.1:8000 {") || !strings.Contains(def, "reverse_proxy 127.0.0.1:8080 {") {
		t.Errorf("empty address overrides must fall back to the defaults:\n%s", def)
	}

	f := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(renderCaddyfile(t, "caddyfile_domain", f)); got != "ops.example.org" {
		t.Errorf("caddyfile_domain = %q, want ops.example.org", got)
	}
	if got := strings.TrimSpace(renderCaddyfile(t, "caddyfile_acme_email", f)); got != "ops@example.org" {
		t.Errorf("caddyfile_acme_email = %q, want ops@example.org", got)
	}
	renderCaddyfile(t, "caddyfile_is_managed", f)
	renderCaddyfile(t, "caddyfile_routes_api", f, "127.0.0.1:9000")
	// The domain of a marker-less operator file is still readable, and it is
	// foreign — bootstrap's refusal keys on exactly that.
	foreign := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(foreign, []byte("legacy.example.com {\n\treverse_proxy 127.0.0.1:3000\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	renderCaddyfile(t, "caddyfile_is_foreign", foreign)
	if got := strings.TrimSpace(renderCaddyfile(t, "caddyfile_domain", foreign)); got != "legacy.example.com" {
		t.Errorf("caddyfile_domain(foreign) = %q", got)
	}
}

// TestCaddyfileMarkerParity — boxdoctor recognises a fleet-managed Caddyfile by
// the same marker the shell renderer writes; the Go constant is a copy, so pin
// it to the lib's CADDY_MARKER line.
func TestCaddyfileMarkerParity(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "lib", "caddyfile.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CADDY_MARKER="(.*)"$`).FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal("scripts/lib/caddyfile.sh must define CADDY_MARKER=\"…\"")
	}
	if m[1] != boxdoctor.CaddyMarker {
		t.Errorf("marker drift: lib %q vs boxdoctor.CaddyMarker %q", m[1], boxdoctor.CaddyMarker)
	}
}

// TestOperatorScriptsShareTheCaddyfileRenderer — bootstrap writes the file,
// update offers to adopt a drifted one, doctor repairs it: all three must go
// through scripts/lib/caddyfile.sh, and none may keep a private copy of the
// layout (the printf that only knew the web tier is how the API stopped
// routing in the first place).
func TestOperatorScriptsShareTheCaddyfileRenderer(t *testing.T) {
	root := repoRootFromTest(t)
	for name, wants := range map[string][]string{
		"bootstrap.sh": {
			`. "$SCRIPT_DIR/lib/caddyfile.sh"`,
			`render_fleet_caddyfile "$WEB_DOMAIN" "${FLEET_ACME_EMAIL:-}"`,
			// A re-run must RELOAD a running caddy, not just `enable --now` it
			// (a no-op on a running unit that left the old routing live).
			"systemctl reload caddy",
			"caddy validate --adapter caddyfile",
		},
		"update.sh": {
			`. "$SCRIPT_DIR/lib/caddyfile.sh"`,
			`caddyfile_is_managed "$CADDYFILE"`,
			`render_fleet_caddyfile "$caddy_domain" "$(caddyfile_acme_email "$CADDYFILE")"`,
			// Same consent rule as unit adoption: --adopt-units, or a TTY yes.
			`elif [[ "$ADOPT_UNITS" == "1" ]]; then`,
			"systemctl reload caddy.service",
			".fleet-backup.",
		},
		"doctor.sh": {
			`. "$SCRIPT_DIR/lib/caddyfile.sh"`,
			`caddyfile_is_managed "$CADDYFILE"`,
			`render_fleet_caddyfile "$caddy_domain" "$(caddyfile_acme_email "$CADDYFILE")"`,
			// --check reports, the default repairs (backup + validate + reload).
			`fail "$CADDYFILE drifted from the shipped layout`,
			`fixed "$CADDYFILE rewritten from the shipped layout`,
			// An operator's own Caddyfile is advisory-only, never rewritten.
			`advise "$CADDYFILE is not fleet-managed`,
			// The end-to-end probe: /api-info THROUGH caddy, pinned to this box.
			`--resolve "${caddy_domain}:443:127.0.0.1" "https://${caddy_domain}/api-info"`,
			`grep -q '"api_version"'`,
		},
	} {
		body, err := os.ReadFile(filepath.Join(root, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s must contain %q", name, want)
			}
		}
		if strings.Contains(string(body), `printf '%s {\n\tencode zstd gzip`) {
			t.Errorf("%s still carries a private Caddyfile printf — render through scripts/lib/caddyfile.sh instead", name)
		}
	}
}

// TestDoctorDryRunPlansCaddyfileCheck — the dry-run checklist must name the
// Caddyfile layout check and the through-caddy /api-info probe, so an operator
// reading the plan learns doctor covers "is the API routing".
func TestDoctorDryRunPlansCaddyfileCheck(t *testing.T) {
	out := runScriptDryRun(t, "doctor.sh", "--dry-run")
	for _, want := range []string{
		"matches scripts/lib/caddyfile.sh",
		"/api-info answers THROUGH caddy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestBootstrapDryRunPlansAPIRouting — with --enable-web --domain the plan
// must state where the API goes, and that a running caddy is reloaded.
func TestBootstrapDryRunPlansAPIRouting(t *testing.T) {
	out := runScriptDryRun(t, "bootstrap.sh", "--dry-run", "--postgres=local", "--enable-web", "--domain", "fleet.example.com")
	for _, want := range []string{
		"would write /etc/caddy/Caddyfile from scripts/lib/caddyfile.sh",
		"/v1/*, /api-info, /.well-known/agent-card.json, /a2a, /triggers/* → orchestrator (127.0.0.1:8000)",
		"/webhooks/* → chat (127.0.0.1:8080)",
		"an already-running caddy is reloaded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}
