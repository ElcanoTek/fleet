package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUnit(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Codex round 2 (#2057): systemd's precedence — the FIRST existing base unit
// in search order, drop-ins from every dir with higher-priority dirs masking
// same-named ones, applied in file-name order, and an empty EnvironmentFile=
// resetting the list.
func TestUnitEnvironmentFilesFollowsSystemdOrder(t *testing.T) {
	root := t.TempDir()
	etc, run, lib := filepath.Join(root, "etc"), filepath.Join(root, "run"), filepath.Join(root, "lib")
	dirs := []string{etc, run, lib}

	// The packaged unit in lib; an admin's full override in etc wins over it.
	writeUnit(t, filepath.Join(lib, "fleet.service"), "[Service]\nEnvironmentFile=/lib/ignored.env\n")
	writeUnit(t, filepath.Join(etc, "fleet.service"), "[Unit]\nEnvironmentFile=/not/in/service.env\n[Service]\n# EnvironmentFile=/commented.env\nEnvironmentFile=-/etc/fleet/fleet.env\n")
	files, found := UnitEnvironmentFiles(dirs, "fleet.service")
	if !found || strings.Join(files, ",") != "/etc/fleet/fleet.env" {
		t.Fatalf("base only: files=%v found=%v, want [/etc/fleet/fleet.env] from the etc unit", files, found)
	}

	// Drop-ins: 10-add (lib) adds; 20-reset (run) resets then adds; a lib
	// 20-reset is masked by run's; 30-more (etc) adds two.
	writeUnit(t, filepath.Join(lib, "fleet.service.d", "10-add.conf"), "[Service]\nEnvironmentFile=/added.env\n")
	writeUnit(t, filepath.Join(lib, "fleet.service.d", "20-reset.conf"), "[Service]\nEnvironmentFile=/masked.env\n")
	writeUnit(t, filepath.Join(run, "fleet.service.d", "20-reset.conf"), "[Service]\nEnvironmentFile=\nEnvironmentFile=/after-reset.env\n")
	writeUnit(t, filepath.Join(etc, "fleet.service.d", "30-more.conf"), "[Service]\nEnvironmentFile=/a.env -/b.env\n")
	files, _ = UnitEnvironmentFiles(dirs, "fleet.service")
	if got := strings.Join(files, ","); got != "/after-reset.env,/a.env,/b.env" {
		t.Fatalf("with drop-ins: %s", got)
	}

	if _, found := UnitEnvironmentFiles(dirs, "absent.service"); found {
		t.Error("an absent unit must report found=false")
	}
}

func TestParseSystemctlEnvironmentFiles(t *testing.T) {
	out := "/etc/fleet/fleet.env (ignore_errors=yes)\n/srv/extra.env (ignore_errors=no)\n\n"
	got := ParseSystemctlEnvironmentFiles(out)
	if strings.Join(got, ",") != "/etc/fleet/fleet.env,/srv/extra.env" {
		t.Fatalf("got %v", got)
	}
}
