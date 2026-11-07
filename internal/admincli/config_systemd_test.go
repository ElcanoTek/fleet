package admincli

import (
	"os"
	"path/filepath"
	"testing"
)

// Codex round 2 (#2057): systemctl's merged EnvironmentFiles view wins when
// systemctl answers; the unit files are parsed (in systemd's search order)
// only when it does not.
func TestEnvFileExportedBySystemdPrefersSystemctl(t *testing.T) {
	t.Setenv("FLEET_SERVICE_NAME", "fleet")
	dir := t.TempDir()
	// On disk the unit exports a different file than systemctl reports.
	if err := os.WriteFile(filepath.Join(dir, "fleet.service"), []byte("[Service]\nEnvironmentFile=/on/disk.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	origDirs, origCtl := systemdUnitDirs, systemctlEnvironmentFiles
	t.Cleanup(func() { systemdUnitDirs, systemctlEnvironmentFiles = origDirs, origCtl })
	systemdUnitDirs = []string{dir}

	var asked string
	systemctlEnvironmentFiles = func(unit string) ([]string, bool) {
		asked = unit
		return []string{"/etc/fleet/fleet.env"}, true
	}
	if !envFileExportedBySystemd("/etc/fleet/fleet.env") || envFileExportedBySystemd("/on/disk.env") {
		t.Error("systemctl's answer must win over the unit files")
	}
	if asked != "fleet.service" {
		t.Errorf("systemctl was asked about %q, want fleet.service", asked)
	}

	systemctlEnvironmentFiles = func(string) ([]string, bool) { return nil, false }
	if !envFileExportedBySystemd("/on/disk.env") || envFileExportedBySystemd("/etc/fleet/fleet.env") {
		t.Error("without systemctl the unit files must be parsed")
	}
}
