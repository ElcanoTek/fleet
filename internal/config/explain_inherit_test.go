package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// FLEET_TITLE_MODEL's documented default is the everyday tier's slug
// (docs/MODEL-DEFAULTS.md checklist step 8).
func TestTitleModelDefaultIsTheCoreModel(t *testing.T) {
	e, _ := envreg.Find("FLEET_TITLE_MODEL")
	if e.Default != DefaultTitleModel {
		t.Fatalf("registry FLEET_TITLE_MODEL Default %q, code DefaultTitleModel %q — update table.go and run make config-docs", e.Default, DefaultTitleModel)
	}
}

// Codex round 3 (#2057) finding 2: an unset setting that inherits its
// default reports the inherited value and the chain it came through,
// resolved with the same function as everything else.
func TestExplainReportsInheritedDefaults(t *testing.T) {
	for _, n := range []string{"FLEET_TITLE_MODEL", "FLEET_METADATA_MODEL", "FLEET_MEMORY_MODEL"} {
		unsetForTest(t, n)
	}
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "FLEET_TITLE_MODEL=vendor/my-model\n")
	xs, err := explainRun(path, []string{"FLEET_MEMORY_MODEL"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	x := xs[0]
	if x.Set || x.Value != "vendor/my-model" || x.Source != SourceDefault ||
		strings.Join(x.InheritedFrom, ",") != "FLEET_METADATA_MODEL,FLEET_TITLE_MODEL" ||
		x.InheritedSource != "env file as FLEET_TITLE_MODEL" {
		t.Fatalf("got %+v", x)
	}
	// Nothing set: the root's own default, still through the chain.
	writeEnv(t, path, "")
	xs, err = explainRun(path, []string{"FLEET_METADATA_MODEL"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if x := xs[0]; x.Value != DefaultTitleModel || strings.Join(x.InheritedFrom, ",") != "FLEET_TITLE_MODEL" || x.InheritedSource != "" {
		t.Fatalf("nothing set: %+v", x)
	}
}

// Codex round 3 (#2057) finding 1: the systemd unit's own environment is a
// source that outranks the shell; explain reports the unit, notes the shell
// value it overrides, honours UnsetEnvironment=, and the env file is not
// read by fleet's own loader when the unit does not point FLEET_ENV_FILE at it.
func TestExplainUsesTheUnitEnvironment(t *testing.T) {
	unsetForTest(t, "FLEET_MAX_COST_USD", "CHAT_MAX_COST_USD", "CUTLASS_MAX_COST_USD", "FLEET_TIMEZONE", "FLEET_ENV_FILE", "FLEET_LOG_LEVEL", "CHAT_LOG_LEVEL", "CUTLASS_LOG_LEVEL")
	dir := t.TempDir()
	envFile := filepath.Join(dir, "fleet.env")
	writeEnv(t, envFile, "FLEET_MAX_COST_USD=12\nFLEET_LOG_LEVEL=warn\n")
	extra := filepath.Join(dir, "extra.env")
	writeEnv(t, extra, "FLEET_TIMEZONE=Europe/Madrid\n")
	show := strings.Join([]string{
		"LoadState=loaded",
		`Environment=GOMEMLIMIT=2GiB "FLEET_TIMEZONE=America/New York"`,
		"EnvironmentFiles=" + envFile + " (ignore_errors=yes)",
		"EnvironmentFiles=" + extra + " (ignore_errors=no)",
		"UnsetEnvironment=FLEET_ENV_FILE FLEET_LOG_LEVEL",
	}, "\n")
	unit := UnitEnvFromSystemctl("fleet.service", show, readFileForTest)
	t.Setenv("FLEET_MAX_COST_USD", "99")     // the operator's shell
	t.Setenv("FLEET_ENV_FILE", "/elsewhere") // removed by the unit

	rep, err := Explain(ExplainOptions{EnvFile: envFile, Keys: []string{"FLEET_MAX_COST_USD", "FLEET_TIMEZONE", "FLEET_LOG_LEVEL", "FLEET_ENV_FILE"}, Unit: unit})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sources.EnvFileReadByLoader {
		t.Error("the unit unsets FLEET_ENV_FILE, so fleet's own loader reads no file")
	}
	by := map[string]Explanation{}
	for _, x := range rep.Settings {
		by[x.Key] = x
	}
	cost := by["FLEET_MAX_COST_USD"]
	if cost.Value != "12" || cost.Source != SourceUnit || cost.SourceDetail != "EnvironmentFile="+envFile {
		t.Errorf("cost = %+v, want 12 from the unit's EnvironmentFile=", cost)
	}
	if !strings.Contains(strings.Join(cost.Notes, "\n"), "your shell sets FLEET_MAX_COST_USD to a different value, but the service uses the systemd unit's") {
		t.Errorf("cost notes %v lack the shell-override note", cost.Notes)
	}
	// A later EnvironmentFile= beats Environment=.
	if tz := by["FLEET_TIMEZONE"]; tz.Value != "Europe/Madrid" || tz.SourceDetail != "EnvironmentFile="+extra {
		t.Errorf("timezone = %+v, want Europe/Madrid from %s", tz, extra)
	}
	// UnsetEnvironment= removes it even though the env file sets it.
	if ll := by["FLEET_LOG_LEVEL"]; ll.Set || ll.Value != "info" {
		t.Errorf("log level = %+v, want the default: the unit unsets it", ll)
	}
	if ef := by["FLEET_ENV_FILE"]; ef.Set || !strings.Contains(strings.Join(ef.Notes, "\n"), "removes it (UnsetEnvironment=)") {
		t.Errorf("FLEET_ENV_FILE = %+v, want unset with a note that the unit removes the shell's value", ef)
	}

	// --no-unit: the shell wins again and the env file is read.
	rep, err = Explain(ExplainOptions{EnvFile: envFile, Keys: []string{"FLEET_MAX_COST_USD"}})
	if err != nil {
		t.Fatal(err)
	}
	if x := rep.Settings[0]; x.Value != "99" || x.Source != SourceProcessEnv || !rep.Sources.EnvFileReadByLoader {
		t.Errorf("without the unit: %+v", x)
	}
}

func TestUnitEnvFromSystemctlNotFound(t *testing.T) {
	if u := UnitEnvFromSystemctl("fleet.service", "LoadState=not-found\nEnvironment=\n", readFileForTest); u != nil {
		t.Fatalf("a unit systemd does not know must be nil, got %+v", u)
	}
}

func TestSplitUnitWords(t *testing.T) {
	got := splitUnitWords(`A=1 "B=two words" 'C=x y' D=e\ f`)
	if strings.Join(got, "|") != "A=1|B=two words|C=x y|D=e f" {
		t.Fatalf("got %q", got)
	}
}

// readFileForTest is os.ReadFile, named for the unit-environment tests.
func readFileForTest(p string) ([]byte, error) { return os.ReadFile(p) }
