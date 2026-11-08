// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sandboxBundle writes a bundle whose manifest names containerfile (empty: no
// sandbox.containerfile key) and whose files are given relative to it.
func sandboxBundle(t *testing.T, containerfile string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	m := "name: test\nsandbox:\n  tag: localhost/fleet-sandbox:latest\n"
	if containerfile != "" {
		m += "  containerfile: \"" + containerfile + "\"  # the bundle's own path\n"
	}
	files["manifest.yaml"] = m
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func usesReleaseImage(t *testing.T, bundle string) bool {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("bash", "-c", `set -u; . "$ROOT/scripts/lib/sandbox-bundle.sh"; fleet_sandbox_uses_release_image "$B" "$ROOT/config/default"`)
	cmd.Env = append(os.Environ(), "ROOT="+root, "B="+bundle)
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return err == nil
}

// TestSandboxContainerfileFollowsTheManifest — the release's generic sandbox
// image is used only for a bundle that ships no Containerfile or the generic
// build context unchanged; a bundle naming its own sandbox.containerfile is
// never mistaken for one that ships none.
func TestSandboxContainerfileFollowsTheManifest(t *testing.T) {
	root := repoRoot(t)
	generic := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, "config/default/sandbox", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := []struct {
		name          string
		containerfile string
		files         map[string]string
		want          bool
	}{
		{"no Containerfile, none named", "", map[string]string{}, true},
		{"generic context at the default path", "", map[string]string{
			"sandbox/Containerfile": generic("Containerfile"), "sandbox/requirements.txt": generic("requirements.txt"), "sandbox/requirements.in": generic("requirements.in"),
		}, true},
		{"custom Containerfile at a custom path", "images/agent/Containerfile", map[string]string{
			"images/agent/Containerfile": "FROM registry.invalid/base\n",
		}, false},
		{"named path missing", "images/agent/Containerfile", map[string]string{}, false},
		{"generic Containerfile, custom requirements", "", map[string]string{
			"sandbox/Containerfile": generic("Containerfile"), "sandbox/requirements.txt": "placeholder==1.0\n", "sandbox/requirements.in": generic("requirements.in"),
		}, false},
		{"custom path holding the generic context", "img/Containerfile", map[string]string{
			"img/Containerfile": generic("Containerfile"), "img/requirements.txt": generic("requirements.txt"), "img/requirements.in": generic("requirements.in"),
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := usesReleaseImage(t, sandboxBundle(t, c.containerfile, c.files)); got != c.want {
				t.Errorf("uses the release image = %v, want %v", got, c.want)
			}
		})
	}

	// build-sandbox-image.sh resolves the same path (--print-tag runs no podman,
	// so read the Containerfile it would use from its missing-file error).
	b := sandboxBundle(t, "images/agent/Containerfile", map[string]string{})
	cmd := exec.Command("bash", filepath.Join(root, "scripts/build-sandbox-image.sh"))
	cmd.Env = append(os.Environ(), "FLEET_CLIENT_CONFIG_DIR="+b)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), filepath.Join(b, "images/agent/Containerfile")) {
		t.Errorf("build-sandbox-image.sh did not resolve the manifest's path:\n%s", out)
	}

	// No script decides from a hard-coded <bundle>/sandbox/Containerfile.
	for _, f := range []string{"update.sh", "bootstrap.sh", "build-sandbox-image.sh"} {
		src, err := os.ReadFile(filepath.Join(root, "scripts", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`/sandbox/Containerfile"`, `config/default/sandbox/Containerfile`} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s hard-codes %s instead of fleet_sandbox_containerfile", f, bad)
			}
		}
	}
}

// TestBuildSandboxImageFallsBackToTheGenericContainerfile — a bundle that
// ships and names no Containerfile customizes nothing, so a by-hand build
// (CI, release.yml, an operator rebuilding) builds the generic bundle's
// Containerfile under the bundle's own sandbox.tag; a bundle that NAMES a
// missing Containerfile is still refused.
func TestBuildSandboxImageFallsBackToTheGenericContainerfile(t *testing.T) {
	root := repoRoot(t)
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "podman.args")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(bundle string) (string, error) {
		_ = os.Remove(argsFile)
		cmd := exec.Command("bash", filepath.Join(root, "scripts/build-sandbox-image.sh"))
		cmd.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"FLEET_CLIENT_CONFIG_DIR="+bundle,
			"FLEET_SERVICE_NAME=fleet-test-no-such-unit",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), err
		}
		args, rerr := os.ReadFile(argsFile)
		if rerr != nil {
			t.Fatalf("podman was not invoked: %v\n%s", rerr, out)
		}
		return string(args), nil
	}

	args, err := run(sandboxBundle(t, "", map[string]string{}))
	if err != nil {
		t.Fatalf("bundle without a Containerfile: %v\n%s", err, args)
	}
	wantCF := filepath.Join(root, "config/default/sandbox/Containerfile")
	if !strings.Contains(args, "-f\n"+wantCF+"\n") {
		t.Errorf("want -f %s, podman got:\n%s", wantCF, args)
	}
	if !strings.Contains(args, "-t\nlocalhost/fleet-sandbox:latest\n") {
		t.Errorf("want the bundle's sandbox.tag, podman got:\n%s", args)
	}

	if out, err := run(sandboxBundle(t, "images/agent/Containerfile", map[string]string{})); err == nil {
		t.Errorf("a bundle naming a missing Containerfile must fail, got podman args:\n%s", out)
	} else if !strings.Contains(out, "missing") {
		t.Errorf("want a missing-Containerfile error, got:\n%s", out)
	}
}

// TestBootstrapBuildsTheSameContainerfile — bootstrap's direct rootless build
// (the --enable-service path) resolves its Containerfile with the helper
// build-sandbox-image.sh uses, so a bundle that ships none builds the generic
// one there too; and a failed build stops a bootstrap that enables the service.
func TestBootstrapBuildsTheSameContainerfile(t *testing.T) {
	root := repoRoot(t)
	resolve := func(bundle string) string {
		t.Helper()
		cmd := exec.Command("bash", "-c", `set -u; . "$ROOT/scripts/lib/sandbox-bundle.sh"; fleet_sandbox_build_containerfile "$B" "$ROOT/config/default"`)
		cmd.Env = append(os.Environ(), "ROOT="+root, "B="+bundle)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got, want := resolve(sandboxBundle(t, "", map[string]string{})), filepath.Join(root, "config/default/sandbox/Containerfile"); got != want {
		t.Errorf("no Containerfile, none named: got %s, want the generic %s", got, want)
	}
	own := sandboxBundle(t, "img/Containerfile", map[string]string{"img/Containerfile": "FROM registry.invalid/base\n"})
	if got := resolve(own); got != filepath.Join(own, "img/Containerfile") {
		t.Errorf("own Containerfile: got %s", got)
	}
	named := sandboxBundle(t, "img/Containerfile", map[string]string{})
	if got := resolve(named); got != filepath.Join(named, "img/Containerfile") {
		t.Errorf("a named, missing Containerfile must come back as is (so the build fails), got %s", got)
	}

	boot := readFile(t, root, "scripts/bootstrap.sh")
	if !strings.Contains(boot, `SANDBOX_BUILD_CF="$(fleet_sandbox_build_containerfile "$CLIENT_CONFIG_DIR" "$REPO_ROOT/config/default")"`) ||
		!strings.Contains(boot, `-f "$SANDBOX_BUILD_CF" "$(dirname "$SANDBOX_BUILD_CF")"`) {
		t.Error("bootstrap's rootless build does not use fleet_sandbox_build_containerfile")
	}
	if strings.Contains(boot, `warn "rootless sandbox build`) {
		t.Error("a failed rootless build while enabling the service must stop bootstrap, not warn")
	}
	if !strings.Contains(readFile(t, root, "scripts/build-sandbox-image.sh"), `fleet_sandbox_build_containerfile "$BUNDLE_DIR" "$REPO_ROOT/config/default"`) {
		t.Error("build-sandbox-image.sh does not use fleet_sandbox_build_containerfile")
	}
}

// shellFunc extracts a top-level `name() { … }` (through its closing `}` at
// column 0) or a one-line `name() { …; }` from a script.
func shellFunc(t *testing.T, script, name string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	for i, ln := range lines {
		if !strings.HasPrefix(ln, name+"() {") {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(ln), "}") {
			return ln + "\n"
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n") + "\n"
			}
		}
	}
	t.Fatalf("function %s() not found", name)
	return ""
}

// ADR-0106 (Codex P2 on #2081): update.sh resolves the bundle's sandbox.image
// from the SERVICE env file. A stale CHAT_SANDBOX_IMAGE line there (or in the
// operator's shell) must read as unset, as it does in the service, so the
// manifest default applies and update takes the tag (pull/build) path instead
// of believing the bundle names a prebuilt image. The canonical
// FLEET_SANDBOX_IMAGE in the env file still wins.
func TestUpdateResolveSandboxImageIgnoresRemovedSpellings(t *testing.T) {
	root := repoRoot(t)
	update := readFile(t, root, "scripts/update.sh")
	fns := shellFunc(t, update, "env_get") + shellFunc(t, update, "service_env_value") + shellFunc(t, update, "resolve_sandbox_image")
	run := func(manifestImage, envFile string) string {
		t.Helper()
		bundle := sandboxBundle(t, "", map[string]string{})
		m := "name: test\nsandbox:\n  image: \"" + manifestImage + "\"\n  tag: localhost/fleet-sandbox:latest\n"
		if err := os.WriteFile(filepath.Join(bundle, "manifest.yaml"), []byte(m), 0o644); err != nil {
			t.Fatal(err)
		}
		env := filepath.Join(t.TempDir(), "fleet.env")
		if err := os.WriteFile(env, []byte(envFile), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", `set -u; . "$ROOT/scripts/lib/sandbox-bundle.sh"; ENV_FILE=/nonexistent; backend_env_file="$E"; `+fns+` resolve_sandbox_image "$B/manifest.yaml"`)
		cmd.Env = append(os.Environ(), "ROOT="+root, "B="+bundle, "E="+env, "CHAT_SANDBOX_IMAGE=shell/stale:1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("resolve_sandbox_image: %v\n%s", err, out)
		}
		return string(out)
	}
	if got := run("${CHAT_SANDBOX_IMAGE:-}", "CHAT_SANDBOX_IMAGE=registry.example/stale:1\n"); got != "" {
		t.Errorf("stale CHAT_SANDBOX_IMAGE resolved to %q; want empty (the tag/build path)", got)
	}
	if got := run("${CHAT_SANDBOX_IMAGE:-registry.example/default:2}", "CHAT_SANDBOX_IMAGE=registry.example/stale:1\n"); got != "registry.example/default:2" {
		t.Errorf("stale CHAT_SANDBOX_IMAGE with a default resolved to %q; want the manifest default", got)
	}
	if got := run("${FLEET_SANDBOX_IMAGE:-}", "FLEET_SANDBOX_IMAGE=registry.example/live:3\n"); got != "registry.example/live:3" {
		t.Errorf("canonical FLEET_SANDBOX_IMAGE resolved to %q; want the env-file value", got)
	}
}

// The three scripts that resolve a manifest sandbox scalar share ONE
// implementation (fleet_sandbox_resolve_scalar), so none can drift back to
// reading a removed spelling on its own (#2081).
func TestSandboxScalarInterpolationIsShared(t *testing.T) {
	root := repoRoot(t)
	for _, s := range []string{"scripts/bootstrap.sh", "scripts/update.sh", "scripts/doctor.sh"} {
		body := readFile(t, root, s)
		if strings.Contains(body, `BASH_REMATCH[3]`) || strings.Contains(body, `${!var`) {
			t.Errorf("%s interpolates a manifest ${VAR} itself; use fleet_sandbox_resolve_scalar (scripts/lib/sandbox-bundle.sh)", s)
		}
		if !strings.Contains(body, "fleet_sandbox_resolve_scalar") {
			t.Errorf("%s no longer resolves its sandbox scalar through fleet_sandbox_resolve_scalar", s)
		}
	}
}

// buildFleet builds the real fleet binary into the test's temp dir (the Go
// build cache makes the second build cheap; the binary is removed with the
// test).
func buildFleet(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the fleet binary")
	}
	bin := filepath.Join(t.TempDir(), "fleet")
	build := exec.Command("go", "build", "-o", bin, "./cmd/fleet")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// writeBundle writes a bundle dir holding manifest.
func writeBundle(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const (
	staleManifest = "name: test\nmcp_servers:\n  - name: s\n    type: http\n    url: \"https://${CHAT_TLS_DOMAIN}/mcp\"\n    always: true\n"
	cleanManifest = "name: test\nsandbox:\n  image: \"${FLEET_SANDBOX_IMAGE:-}\"\n"
)

// runBundlesCheck runs fleet_retired_bundles_check BIN DIR… and returns its
// exit code and stderr.
func runBundlesCheck(t *testing.T, bin string, dirs ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", `. "$ROOT/scripts/lib/retired-bundle.sh"; fleet_retired_bundles_check "$@"`, "bash", bin}, dirs...)...)
	cmd.Env = append(os.Environ(), "ROOT="+repoRoot(t), "FLEET_ENV_FILE=/nonexistent")
	var errb strings.Builder
	cmd.Stderr = &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), errb.String()
}

// fakeFleet writes a stand-in binary that prints msg and exits rc.
func fakeFleet(t *testing.T, rc int, msg string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fleet")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' '"+msg+"' >&2\nexit "+strconv.Itoa(rc)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// ADR-0106 (Codex P2 on #2081): every outcome of the pre-install scan. A
// finding refuses (1) with the list; a failed scan aborts (2) with the
// scanner's output; ONLY a binary that rejects the flag itself (usage exit 2
// naming -retired-only) is skipped; a clean bundle passes.
func TestRetiredBundlesCheckBranches(t *testing.T) {
	bin := buildFleet(t)
	if rc, out := runBundlesCheck(t, bin, writeBundle(t, cleanManifest)); rc != 0 {
		t.Errorf("clean: rc=%d\n%s", rc, out)
	}
	if rc, out := runBundlesCheck(t, bin, writeBundle(t, staleManifest)); rc != 1 || !strings.Contains(out, "mcp_servers[0].url references the removed variable ${CHAT_TLS_DOMAIN}; rename it to ${FLEET_TLS_DOMAIN}") {
		t.Errorf("finding: rc=%d\n%s", rc, out)
	}
	if rc, out := runBundlesCheck(t, bin, writeBundle(t, "mcp_servers: [unclosed\n")); rc != 2 || !strings.Contains(out, "failed") {
		t.Errorf("scan error: rc=%d\n%s", rc, out)
	}
	if rc, out := runBundlesCheck(t, fakeFleet(t, 2, "flag provided but not defined: -retired-only"), writeBundle(t, staleManifest)); rc != 0 || !strings.Contains(out, "predates the removed-variable check") {
		t.Errorf("unsupported flag: rc=%d\n%s", rc, out)
	}
	if rc, out := runBundlesCheck(t, fakeFleet(t, 2, "some other usage problem"), writeBundle(t, cleanManifest)); rc != 2 {
		t.Errorf("a usage error that is not the missing flag must abort: rc=%d\n%s", rc, out)
	}
	if rc, out := runBundlesCheck(t, fakeFleet(t, 139, "segfault"), writeBundle(t, cleanManifest)); rc != 2 {
		t.Errorf("a crash must abort: rc=%d\n%s", rc, out)
	}
	// A finding and a scan error together: the scan error wins (abort).
	if rc, _ := runBundlesCheck(t, bin, writeBundle(t, staleManifest), writeBundle(t, "mcp_servers: [unclosed\n")); rc != 2 {
		t.Errorf("finding + error: rc=%d, want 2", rc)
	}
}

// ADR-0106 (Codex P2 on #2081): fleet update scans EVERY bundle the restarted
// service could load — the service env file's and, when it differs, the one
// this run updated (an explicit --client-config) — and refuses before
// installing when either is stale. Here only the service's bundle is stale.
// It runs update.sh's own fleet_update_retired_scan with the real binary,
// and pins that update.sh calls it before `release install`.
func TestUpdateRefusesARetiredBundleBeforeInstalling(t *testing.T) {
	bin := buildFleet(t)
	root := repoRoot(t)
	update := readFile(t, root, "scripts/update.sh")
	fns := shellFunc(t, update, "env_get") + shellFunc(t, update, "fleet_update_retired_scan")
	run := func(svcManifest, explicitManifest string) (installed bool, stderr string) {
		t.Helper()
		svc, explicit := writeBundle(t, svcManifest), writeBundle(t, explicitManifest)
		env := filepath.Join(t.TempDir(), "fleet.env")
		if err := os.WriteFile(env, []byte("FLEET_CLIENT_CONFIG_DIR="+svc+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(t.TempDir(), "installed")
		cmd := exec.Command("bash", "-c", `set -euo pipefail
. "$ROOT/scripts/lib/retired-bundle.sh"
die() { printf '%s\n' "$*" >&2; exit 1; }
ENV_FILE=/nonexistent backend_env_file="$E" NEW_FLEET="$BIN" CLIENT_DIR="$C" SERVICE_NAME=fleet
`+fns+`
fleet_update_retired_scan
touch "$MARKER"`)
		cmd.Env = append(os.Environ(), "ROOT="+root, "BIN="+bin, "E="+env, "C="+explicit, "MARKER="+marker, "FLEET_ENV_FILE=/nonexistent")
		var errb strings.Builder
		cmd.Stderr = &errb
		_ = cmd.Run()
		_, statErr := os.Stat(marker)
		return statErr == nil, errb.String()
	}
	installed, stderr := run(staleManifest, cleanManifest)
	if installed {
		t.Error("update installed over a stale service bundle (the explicit bundle was clean)")
	}
	for _, want := range []string{"references the removed variable ${CHAT_TLS_DOMAIN}; rename it to ${FLEET_TLS_DOMAIN}", "nothing was installed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal lacks %q:\n%s", want, stderr)
		}
	}
	if installed, stderr := run(cleanManifest, staleManifest); installed {
		t.Errorf("update installed over a stale explicit bundle:\n%s", stderr)
	}
	if installed, stderr := run(cleanManifest, cleanManifest); !installed {
		t.Errorf("clean bundles were refused:\n%s", stderr)
	}

	check, install := strings.Index(update, "\n  fleet_update_retired_scan\n"), strings.Index(update, `"$NEW_FLEET" release install`)
	if check < 0 || install < 0 || check > install {
		t.Error("update.sh must run fleet_update_retired_scan before `release install`")
	}
}

// ADR-0106 (Codex P2 on #2081): bootstrap scans the bundle right after the
// release is fetched and verified (both read-only) and BEFORE its first
// host-changing step, so "nothing was installed and the host was not
// changed" is true.
func TestBootstrapScansTheBundleBeforeChangingTheHost(t *testing.T) {
	boot := readFile(t, repoRoot(t), "scripts/bootstrap.sh")
	scan := strings.Index(boot, `    fleet_bootstrap_scan_bundle "$CLIENT_CONFIG_DIR"`)
	fetch := strings.Index(boot, `FLEET_RELEASE_BIN="$(fleet_release_fetch`)
	if scan < 0 || fetch < 0 || fetch > scan {
		t.Fatal("bootstrap.sh must fetch the release, then scan the bundle")
	}
	for _, mutating := range []string{
		`step "Installing system dependencies`, "\ncockpit_uninstall\n", "\nssh_key_only\n",
		`step "Ensuring credential env file`, `step "Resolving client config`, `step "Installing the fleet binary`,
	} {
		at := strings.Index(boot, mutating)
		if at < 0 {
			t.Errorf("bootstrap.sh no longer has %q; update this test", mutating)
			continue
		}
		if at < scan {
			t.Errorf("bootstrap.sh changes the host (%q) before the removed-variable scan", strings.TrimSpace(mutating))
		}
	}
}
