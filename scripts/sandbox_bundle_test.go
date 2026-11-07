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
