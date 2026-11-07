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
// ships and names no Containerfile customizes nothing, so an on-box build
// (--from-source, or no release image reachable) builds the generic bundle's
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
