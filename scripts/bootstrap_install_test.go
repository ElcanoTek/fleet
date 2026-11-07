// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBuiltBinaryInstallKeepsFleetPrevOnAnIdenticalReinstall — installing the
// bytes that are already live swaps nothing and leaves fleet.prev (the
// rollback target) exactly as it was; a different binary becomes live with
// the old one kept as fleet.prev.
func TestBuiltBinaryInstallKeepsFleetPrevOnAnIdenticalReinstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "opt")
	src := t.TempDir()
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	install := func(body string) {
		t.Helper()
		p := filepath.Join(src, "fleet")
		write(p, body)
		if out, err := runReleaseLib(t, []string{"SRC=" + p, "DIR=" + dir}, `fleet_install_built_binary "$SRC" "$DIR"`); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	install("OLD")
	install("NEW")
	if read(filepath.Join(dir, "fleet")) != "NEW" || read(filepath.Join(dir, "fleet.prev")) != "OLD" {
		t.Fatal("a new binary must go live with the old one kept as fleet.prev")
	}
	install("NEW") // the same bytes again
	if got := read(filepath.Join(dir, "fleet.prev")); got != "OLD" {
		t.Fatalf("an identical reinstall overwrote fleet.prev with %q — rollback would restore the live binary", got)
	}
}

// TestBootstrapInstallsAReleaseThroughTheGoInstall — bootstrap installs a
// release with `fleet release install --no-restart` (the one install path,
// which keeps fleet.prev on an identical reinstall) and never hand-links
// fleet.prev itself.
func TestBootstrapInstallsAReleaseThroughTheGoInstall(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !regexp.MustCompile(`"\$FLEET_RELEASE_BIN" release install --tag "\$RELEASE_TAG" --dir "\$RELEASE_DIR" \\\n\s+--install-dir "\$INSTALL_DIR" --service "\$SERVICE_NAME" --no-restart`).MatchString(src) {
		t.Error("bootstrap does not install the release with `fleet release install --no-restart`")
	}
	if strings.Contains(src, `fleet.prev"`) && regexp.MustCompile(`ln -f [^\n]*fleet\.prev`).MatchString(src) {
		t.Error("bootstrap links fleet.prev itself instead of going through the shared install")
	}
}
