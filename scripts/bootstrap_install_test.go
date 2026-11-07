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
