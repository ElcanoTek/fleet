// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
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
	out, err := runCaddyfileFn(t, nil, fn, args...)
	if err != nil {
		t.Fatalf("%s %v failed: %v\n--- output ---\n%s", fn, args, err, out)
	}
	return out
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

// runCaddyfileFn is renderCaddyfile without the exit-status assertion, for
// predicates whose "false" is a non-zero exit. env entries are appended to
// the process environment (a PATH entry here shadows the real one).
func runCaddyfileFn(t *testing.T, env []string, fn string, args ...string) (string, error) {
	t.Helper()
	root := repoRootFromTest(t)
	lib := filepath.Join(root, "scripts", "lib", "caddyfile.sh")
	cmd := exec.Command("bash", append([]string{"-c", ". " + lib + "; " + fn + ` "$@"`, "caddyfile"}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stubPackageManagers writes fake rpm, dpkg and dpkg-query binaries into a
// temp dir and returns a PATH entry that shadows the real ones. rpmDigest is
// what `rpm -q --dump caddy` reports for /etc/caddy/Caddyfile ("" = rpm owns
// no such file); dpkgMD5 is what dpkg's Conffiles record says ("" = dpkg owns
// no such file). The dump includes a directory row with a zero digest, as a
// real dump does, so the awk match has something to skip.
func stubPackageManagers(t *testing.T, rpmDigest, dpkgMD5 string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rpm := "#!/usr/bin/env bash\ncase \"$1\" in\n"
	if rpmDigest != "" {
		rpm += "  -qf) echo caddy ;;\n" +
			"  -q) [[ \"$2\" == --dump ]] && { echo '/etc/caddy/Caddyfile.d 4096 1700000000 0000000000000000000000000000000000000000000000000000000000000000 040755 root root 0 0 0 X'; echo '/etc/caddy/Caddyfile 215 1700000000 " + rpmDigest + " 0100644 root root 1 0 0 X'; } ;;\n"
	} else {
		rpm += "  -qf) exit 1 ;;\n"
	}
	rpm += "esac\n"
	write("rpm", rpm)
	if dpkgMD5 != "" {
		write("dpkg", "#!/usr/bin/env bash\n[[ \"$1\" == -S ]] && echo 'caddy: /etc/caddy/Caddyfile'\n")
		write("dpkg-query", "#!/usr/bin/env bash\nprintf ' /etc/caddy/Caddyfile "+dpkgMD5+"\\n /etc/caddy/Caddyfile.old deadbeefdeadbeefdeadbeefdeadbeef obsolete\\n'\n")
	} else {
		write("dpkg", "#!/usr/bin/env bash\nexit 1\n")
		write("dpkg-query", "#!/usr/bin/env bash\nexit 1\n")
	}
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestCaddyfilePackageDefaultIsNotForeign is #1657: the caddy package's
// untouched default Caddyfile — recognised by the digest the package manager
// recorded for it — is not "someone else's config", while the same file with
// one edit still is, and a file no package knows stays foreign as before.
// rpm and dpkg are stubbed on PATH so both lookups actually run.
func TestCaddyfilePackageDefaultIsNotForeign(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	stock := "# The Caddyfile is an easy way to configure your Caddy web server.\nhttp:// {\n\troot * /usr/share/caddy\n\tfile_server\n}\nimport Caddyfile.d/*.caddyfile\n"
	f := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(f, []byte(stock), 0o644); err != nil {
		t.Fatal(err)
	}
	sha := sha256.Sum256([]byte(stock))
	md := md5.Sum([]byte(stock))
	viaRPM := []string{stubPackageManagers(t, hex.EncodeToString(sha[:]), "")}
	viaDpkg := []string{stubPackageManagers(t, "", hex.EncodeToString(md[:]))}
	viaNone := []string{stubPackageManagers(t, "", "")}

	for name, env := range map[string][]string{"rpm sha256": viaRPM, "dpkg md5": viaDpkg} {
		if out, err := runCaddyfileFn(t, env, "caddyfile_is_package_default", f); err != nil {
			t.Fatalf("%s: the packaged default was not recognised: %v\n%s", name, err, out)
		}
		if out, err := runCaddyfileFn(t, env, "caddyfile_is_foreign", f); err == nil {
			t.Fatalf("%s: the packaged default was treated as foreign:\n%s", name, out)
		}
	}
	// No package knows the file: the pre-#1657 rule — unmarked and non-empty
	// means foreign.
	if _, err := runCaddyfileFn(t, viaNone, "caddyfile_is_foreign", f); err != nil {
		t.Fatal("an unmarked file no package knows was not treated as foreign")
	}
	// rpm installed on a dpkg box: for a file it does not own, real rpm
	// prints "is not owned by any package" to stdout and exits 1. The Go
	// harness runs bash without pipefail, so this pins that the rpm branch
	// reads rpm's own exit status and falls through to dpkg.
	rpmNotOwned := filepath.Join(t.TempDir(), "rpm")
	if err := os.WriteFile(rpmNotOwned, []byte("#!/usr/bin/env bash\necho \"file $3 is not owned by any package\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	viaDpkgWithRPM := []string{"PATH=" + filepath.Dir(rpmNotOwned) + string(os.PathListSeparator) + strings.TrimPrefix(viaDpkg[0], "PATH=")}
	if out, err := runCaddyfileFn(t, viaDpkgWithRPM, "caddyfile_is_package_default", f); err != nil {
		t.Fatalf("dpkg box with an rpm that owns nothing: the packaged default was not recognised: %v\n%s", err, out)
	}
	// Untouched is not empty: the stock file imports Caddyfile.d/*.caddyfile,
	// where Fedora tells operators to put their sites. An empty drop-in dir
	// leaves it replaceable; one drop-in site makes it foreign, because
	// fleet's rendered file has no import and replacing it would drop that site.
	dropins := filepath.Join(filepath.Dir(f), "Caddyfile.d")
	if err := os.MkdirAll(dropins, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string][]string{"rpm sha256": viaRPM, "dpkg md5": viaDpkg} {
		if out, err := runCaddyfileFn(t, env, "caddyfile_is_foreign", f); err == nil {
			t.Fatalf("%s: the packaged default with an empty Caddyfile.d was treated as foreign:\n%s", name, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dropins, "legacy.caddyfile"), []byte("legacy.example.com {\n\treverse_proxy 127.0.0.1:3000\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string][]string{"rpm sha256": viaRPM, "dpkg md5": viaDpkg} {
		if _, err := runCaddyfileFn(t, env, "caddyfile_is_package_default", f); err == nil {
			t.Fatalf("%s: a packaged default importing a drop-in site was recognised as replaceable", name)
		}
		if _, err := runCaddyfileFn(t, env, "caddyfile_is_foreign", f); err != nil {
			t.Fatalf("%s: a packaged default importing a drop-in site was not treated as foreign", name)
		}
	}
	if err := os.RemoveAll(dropins); err != nil {
		t.Fatal(err)
	}
	// A site config package owning the path is not the caddy package: its
	// untouched file is the operator's config, so it stays foreign.
	otherPkg := filepath.Join(t.TempDir(), "rpm")
	if err := os.WriteFile(otherPkg, []byte("#!/usr/bin/env bash\ncase \"$1\" in\n  -qf) echo acme-caddy-sites ;;\n  -q) echo '/etc/caddy/Caddyfile 215 1700000000 "+hex.EncodeToString(sha[:])+" 0100644 root root 1 0 0 X' ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	viaOtherPkg := []string{"PATH=" + filepath.Dir(otherPkg) + string(os.PathListSeparator) + os.Getenv("PATH")}
	if _, err := runCaddyfileFn(t, viaOtherPkg, "caddyfile_is_foreign", f); err != nil {
		t.Fatal("an untouched Caddyfile owned by a non-caddy package was not treated as foreign")
	}
	// One operator edit and it is a real config again, whatever the package says.
	if err := os.WriteFile(f, []byte(stock+"legacy.example.com {\n\treverse_proxy 127.0.0.1:3000\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string][]string{"rpm sha256": viaRPM, "dpkg md5": viaDpkg} {
		if _, err := runCaddyfileFn(t, env, "caddyfile_is_foreign", f); err != nil {
			t.Fatalf("%s: an edited Caddyfile was not treated as foreign", name)
		}
	}
	// A marked (fleet-managed) file is never the package default.
	if err := os.WriteFile(f, []byte("# Managed by fleet (scripts/bootstrap.sh) — re-runs overwrite this file.\n"+stock), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCaddyfileFn(t, viaRPM, "caddyfile_is_foreign", f); err == nil {
		t.Fatal("a fleet-managed file was treated as foreign")
	}
}

// TestCaddyTrustedProxiesState — the predicate bootstrap, doctor and update
// all decide on: only an ABSENT FLEET_TRUSTED_PROXIES is "unset" (the gap they
// fill); the loopback pair in any order/spacing/quoting is "ok"; anything else
// — including an explicit empty value — is the operator's "other", which none
// of them rewrites. Last assignment wins, as systemd's EnvironmentFile= does.
func TestCaddyTrustedProxiesState(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body, want string
	}{
		{"no file", "", "unset"},
		{"key absent", "OPENROUTER_API_KEY=x\n# FLEET_TRUSTED_PROXIES=127.0.0.1,::1\n", "unset"},
		{"shipped value", "FLEET_TRUSTED_PROXIES=127.0.0.1,::1\n", "ok"},
		{"reordered, spaced, quoted", "FLEET_TRUSTED_PROXIES=\"::1, 127.0.0.1\"\n", "ok"},
		{"explicit empty is the operator's", "FLEET_TRUSTED_PROXIES=\n", "other"},
		{"v4 only", "FLEET_TRUSTED_PROXIES=127.0.0.1\n", "other"},
		{"an extra proxy", "FLEET_TRUSTED_PROXIES=127.0.0.1,::1,10.0.0.5\n", "other"},
		{"last assignment wins", "FLEET_TRUSTED_PROXIES=10.0.0.5\nFLEET_TRUSTED_PROXIES=127.0.0.1,::1\n", "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".env")
			if tc.body != "" {
				if err := os.WriteFile(f, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.TrimSpace(renderCaddyfile(t, "caddy_trusted_proxies_state", f)); got != tc.want {
				t.Errorf("caddy_trusted_proxies_state = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCaddyTrustedProxiesValueParity — the value the scripts write is the one
// deploy/Caddyfile and deploy/fleet.service tell an operator about, and it
// names exactly the loopback peers the rendered Caddyfile dials from.
func TestCaddyTrustedProxiesValueParity(t *testing.T) {
	root := repoRootFromTest(t)
	lib, err := os.ReadFile(filepath.Join(root, "scripts", "lib", "caddyfile.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CADDY_TRUSTED_PROXIES="(.*)"$`).FindStringSubmatch(string(lib))
	if m == nil {
		t.Fatal(`scripts/lib/caddyfile.sh must define CADDY_TRUSTED_PROXIES="…"`)
	}
	if m[1] != "127.0.0.1,::1" {
		t.Errorf("CADDY_TRUSTED_PROXIES = %q, want the loopback pair 127.0.0.1,::1", m[1])
	}
	for _, f := range []string{"Caddyfile", "fleet.service"} {
		body, err := os.ReadFile(filepath.Join(root, "deploy", f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "FLEET_TRUSTED_PROXIES="+m[1]) {
			t.Errorf("deploy/%s must name FLEET_TRUSTED_PROXIES=%s (the value bootstrap writes)", f, m[1])
		}
	}
}

// TestOperatorScriptsSetTrustedProxies — bootstrap writes FLEET_TRUSTED_PROXIES
// with the Caddyfile; doctor (repair; --check reports) and update (consent:
// --adopt-units or a TTY yes) add it to an older fleet-managed front. All three
// decide through the shared predicate and write only when it says "unset".
func TestOperatorScriptsSetTrustedProxies(t *testing.T) {
	root := repoRootFromTest(t)
	for name, wants := range map[string][]string{
		"bootstrap.sh": {
			`if ! caddy_upstreams_loopback "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)"; then`,
			`elif [[ "$(caddy_trusted_proxies_state "$ENV_FILE")" == "unset" ]]; then`,
			`upsert_env FLEET_TRUSTED_PROXIES "$CADDY_TRUSTED_PROXIES"`,
			// The restart that applies it is checked, not swallowed.
			`if systemctl try-restart "$SERVICE_NAME" >/dev/null 2>&1; then`,
			`but restarting ${SERVICE_NAME} FAILED`,
		},
		"doctor.sh": {
			`trusted_state="$(caddy_trusted_proxies_state "$ENV_FILE")"`,
			`if ! caddy_upstreams_loopback "$(env_get FLEET_SERVER_ADDR)" "$(env_get FLEET_ORCHESTRATOR_ADDR)"; then`,
			`and Caddy proxies to a non-loopback backend`,
			`fail "FLEET_TRUSTED_PROXIES unset in $ENV_FILE`,
			`upsert_env_key "$ENV_FILE" FLEET_TRUSTED_PROXIES "$CADDY_TRUSTED_PROXIES"`,
			`fixed "FLEET_TRUSTED_PROXIES=${CADDY_TRUSTED_PROXIES} added to $ENV_FILE`,
			`advise "FLEET_TRUSTED_PROXIES is set to '${trusted_val}'`,
		},
		"update.sh": {
			`&& ! caddy_upstreams_loopback "$(env_get FLEET_SERVER_ADDR "$backend_env_file")" "$(env_get FLEET_ORCHESTRATOR_ADDR "$backend_env_file")"; then`,
			`"$(caddy_trusted_proxies_state "$backend_env_file")" == "unset" ]]; then`,
			`upsert_env_file "$backend_env_file" FLEET_TRUSTED_PROXIES "$CADDY_TRUSTED_PROXIES"`,
		},
	} {
		body, err := os.ReadFile(filepath.Join(root, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		s := string(body)
		for _, want := range wants {
			if !strings.Contains(s, want) {
				t.Errorf("%s must contain %q", name, want)
			}
		}
		if strings.Contains(s, `systemctl try-restart "$SERVICE_NAME" >/dev/null 2>&1 || true
    ok "FLEET_TRUSTED_PROXIES`) {
			t.Errorf("%s swallows the restart that applies FLEET_TRUSTED_PROXIES and reports success anyway", name)
		}
		// Only ever written through the predicate: no unconditional write.
		if n, m := strings.Count(s, "FLEET_TRUSTED_PROXIES \"$CADDY_TRUSTED_PROXIES\""), strings.Count(s, "caddy_trusted_proxies_state"); n == 0 || m < n {
			t.Errorf("%s: %d writes of FLEET_TRUSTED_PROXIES but %d state checks — every write must be gated on the predicate", name, n, m)
		}
	}
	// update's write sits under the same consent gate as the Caddyfile rewrite.
	upd, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	block := sliceBetween(t, string(upd), `"$(caddy_trusted_proxies_state "$backend_env_file")" == "unset" ]]; then`, "# ── offer to install missing scheduled-maintenance timer pairs")
	for _, want := range []string{`elif [[ "$ADOPT_UNITS" == "1" ]]; then`, `elif [[ -t 0 && "$ASSUME_YES" != "1" ]]; then`, `if [[ "$do_adopt" == "1" ]]; then`} {
		if !strings.Contains(block, want) {
			t.Errorf("update.sh FLEET_TRUSTED_PROXIES block must gate on consent (missing %q)", want)
		}
	}
}

// TestBootstrapDryRunPlansTrustedProxies — with --domain the plan says it will
// write FLEET_TRUSTED_PROXIES when the env file lacks it, and that it leaves
// an operator's value alone when present.
func TestBootstrapDryRunPlansTrustedProxies(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	args := []string{"--dry-run", "--postgres=local", "--enable-web", "--domain", "fleet.example.com"}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", args...)
	if err != nil {
		t.Fatalf("bootstrap --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if !strings.Contains(out, "would set FLEET_TRUSTED_PROXIES=127.0.0.1,::1 in "+envFile) {
		t.Errorf("plan must say it sets FLEET_TRUSTED_PROXIES for the Caddy front\n--- output ---\n%s", out)
	}
	if err := os.WriteFile(envFile, []byte("FLEET_TRUSTED_PROXIES=10.0.0.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", args...)
	if err != nil {
		t.Fatalf("bootstrap --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if strings.Contains(out, "would set FLEET_TRUSTED_PROXIES") || !strings.Contains(out, "FLEET_TRUSTED_PROXIES already set") {
		t.Errorf("an operator's FLEET_TRUSTED_PROXIES must be left alone\n--- output ---\n%s", out)
	}
}

// TestCaddyUpstreamsLoopback — 127.0.0.1,::1 is the right FLEET_TRUSTED_PROXIES
// only while Caddy dials both Go listeners over loopback. Empty arguments mean
// the renderer's defaults (callers pass env-file values verbatim); a listener
// on an interface address — or on every interface — is not loopback.
func TestCaddyUpstreamsLoopback(t *testing.T) {
	for _, tc := range []struct {
		chat, orch string
		want       bool
	}{
		{"", "", true},
		{"127.0.0.1:9080", "127.0.0.1:9000", true},
		{"[::1]:8080", "localhost:8000", true},
		{"10.0.0.5:8080", "", false},
		{"", "192.0.2.10:8000", false},
		{"0.0.0.0:8080", "", false},
		{":8080", "", false},
		{"", "[2001:db8::1]:8000", false},
	} {
		_, err := runCaddyfileFn(t, nil, "caddy_upstreams_loopback", tc.chat, tc.orch)
		if got := err == nil; got != tc.want {
			t.Errorf("caddy_upstreams_loopback(%q, %q) = %v, want %v", tc.chat, tc.orch, got, tc.want)
		}
	}
}

// TestBootstrapDryRunNonLoopbackUpstreamWritesNothing — a backend listener
// moved off loopback means Caddy connects from an interface address, so the
// plan must not claim it writes 127.0.0.1,::1.
func TestBootstrapDryRunNonLoopbackUpstreamWritesNothing(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_ORCHESTRATOR_ADDR=10.0.0.5:8000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh",
		"--dry-run", "--postgres=local", "--enable-web", "--domain", "fleet.example.com")
	if err != nil {
		t.Fatalf("bootstrap --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if strings.Contains(out, "would set FLEET_TRUSTED_PROXIES") || !strings.Contains(out, "would NOT write FLEET_TRUSTED_PROXIES") {
		t.Errorf("non-loopback upstream: plan must not write FLEET_TRUSTED_PROXIES\n--- output ---\n%s", out)
	}
}
