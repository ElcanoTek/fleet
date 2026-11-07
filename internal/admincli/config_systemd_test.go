package admincli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubSystemd points the unit lookup at dir (for the file-parsing fallback)
// and makes systemctl answer show (ok=false: systemctl unavailable).
func stubSystemd(t *testing.T, dir, show string, ok bool) (asked *string) {
	t.Helper()
	t.Setenv("FLEET_SERVICE_NAME", "fleet")
	origDirs, origShow := systemdUnitDirs, systemctlShow
	t.Cleanup(func() { systemdUnitDirs, systemctlShow = origDirs, origShow })
	systemdUnitDirs = []string{dir}
	var unit string
	systemctlShow = func(u string) (string, bool) { unit = u; return show, ok }
	return &unit
}

// Codex round 3 (#2057): systemctl's merged view wins when systemctl
// answers; the unit files are parsed (in systemd's search order) only when
// it does not; a unit systemd does not know is no unit.
func TestLoadUnitEnvPrefersSystemctl(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_TIMEZONE=UTC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fleet.service"), []byte("[Service]\nEnvironment=FLEET_TIMEZONE=from-disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	asked := stubSystemd(t, dir, "LoadState=loaded\nEnvironment=FLEET_TIMEZONE=from-systemctl\nEnvironmentFiles=\nUnsetEnvironment=\n", true)
	u := loadUnitEnv()
	if u == nil || u.Env["FLEET_TIMEZONE"] != "from-systemctl" || *asked != "fleet.service" {
		t.Fatalf("systemctl answer: %+v (asked %q)", u, *asked)
	}

	stubSystemd(t, dir, "", false)
	if u := loadUnitEnv(); u == nil || u.Env["FLEET_TIMEZONE"] != "from-disk" {
		t.Fatalf("without systemctl the unit files must be parsed: %+v", u)
	}

	stubSystemd(t, dir, "LoadState=not-found\n", true)
	if u := loadUnitEnv(); u != nil {
		t.Fatalf("an unknown unit is no unit: %+v", u)
	}
}

// Codex round 3 (#2057) finding 1, end to end with fake systemctl output: the
// unit's value outranks the shell's, the table and JSON say so, and
// --no-unit leaves the unit out.
func TestConfigExplainMergesTheUnitEnvironment(t *testing.T) {
	clearEnvNames(t, "FLEET_TIMEZONE")
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.env")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stubSystemd(t, dir, "LoadState=loaded\nEnvironment=FLEET_TIMEZONE=Europe/Madrid\nEnvironmentFiles="+path+" (ignore_errors=yes)\nUnsetEnvironment=FLEET_ENV_FILE\n", true)
	t.Setenv("FLEET_TIMEZONE", "Asia/Tokyo") // the operator's shell

	table := captureStdout(t, func() { configExplain([]string{"FLEET_TIMEZONE", "--env-file", path}) })
	for _, want := range []string{
		"systemd unit fleet.service (EnvironmentFile=" + path + ")",
		"Europe/Madrid",
		"systemd unit (Environment=)",
		"your shell sets FLEET_TIMEZONE to a different value, but the service uses the systemd unit's",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}

	js := captureStdout(t, func() { configExplain([]string{"FLEET_TIMEZONE", "--json", "--env-file", path}) })
	var doc struct {
		Sources struct {
			Unit *struct {
				Unit string `json:"unit"`
			} `json:"systemd_unit"`
		} `json:"sources"`
		Settings []struct {
			Value        string `json:"configured_value"`
			Source       string `json:"source"`
			SourceDetail string `json:"source_detail"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, js)
	}
	if doc.Sources.Unit == nil || doc.Sources.Unit.Unit != "fleet.service" ||
		doc.Settings[0].Value != "Europe/Madrid" || doc.Settings[0].Source != "systemd unit" || doc.Settings[0].SourceDetail != "Environment=" {
		t.Errorf("json: %+v", doc)
	}

	noUnit := captureStdout(t, func() { configExplain([]string{"FLEET_TIMEZONE", "--no-unit", "--env-file", path}) })
	if !strings.Contains(noUnit, "Asia/Tokyo") || strings.Contains(noUnit, "systemd unit") {
		t.Errorf("--no-unit must leave the unit out:\n%s", noUnit)
	}
}
