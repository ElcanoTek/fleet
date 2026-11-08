// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// ADR-0106 (redesign on #2081): fleet update refuses a client bundle that
// still uses a removed CHAT_/CUTLASS_ variable BEFORE installing anything, by
// asking the NEW binary's read-only scan. This builds the real binary, runs
// the shared check (scripts/lib/retired-bundle.sh) the way update.sh does,
// and confirms the refusal stops the run before the install step; a clean
// bundle proceeds. It also pins that update.sh runs the check before
// `release install`.
func TestUpdateRefusesARetiredBundleBeforeInstalling(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the fleet binary")
	}
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "fleet")
	build := exec.Command("go", "build", "-o", bin, "./cmd/fleet")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := func(manifest string) (installed bool, stderr string) {
		t.Helper()
		bundle := sandboxBundle(t, "", map[string]string{})
		if err := os.WriteFile(filepath.Join(bundle, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(t.TempDir(), "installed")
		cmd := exec.Command("bash", "-c", `set -euo pipefail
. "$ROOT/scripts/lib/retired-bundle.sh"
die() { printf '%s\n' "$*" >&2; exit 1; }
fleet_retired_bundle_check "$BIN" "$B" || die "nothing was installed; rename these in the client bundle and re-run"
touch "$MARKER"`)
		cmd.Env = append(os.Environ(), "ROOT="+root, "BIN="+bin, "B="+bundle, "MARKER="+marker, "FLEET_ENV_FILE=/nonexistent")
		var errb strings.Builder
		cmd.Stderr = &errb
		_ = cmd.Run()
		_, statErr := os.Stat(marker)
		return statErr == nil, errb.String()
	}
	installed, stderr := run("name: test\nmcp_servers:\n  - name: s\n    type: http\n    url: \"https://${CHAT_TLS_DOMAIN}/mcp\"\n    always: true\n")
	if installed {
		t.Error("a bundle using a removed variable was installed over")
	}
	for _, want := range []string{"mcp_servers[0].url references the removed variable ${CHAT_TLS_DOMAIN}; rename it to ${FLEET_TLS_DOMAIN}", "nothing was installed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal lacks %q:\n%s", want, stderr)
		}
	}
	if installed, stderr := run("name: test\nsandbox:\n  image: \"${FLEET_SANDBOX_IMAGE:-}\"\n"); !installed {
		t.Errorf("a clean bundle was refused:\n%s", stderr)
	}

	update := readFile(t, root, "scripts/update.sh")
	check, install := strings.Index(update, `fleet_retired_bundle_check "$NEW_FLEET"`), strings.Index(update, `"$NEW_FLEET" release install`)
	if check < 0 || install < 0 || check > install {
		t.Error("update.sh must run fleet_retired_bundle_check on the new binary before `release install`")
	}
}
