package admincli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearEnvNames(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		t.Setenv(n, "")
		_ = os.Unsetenv(n)
	}
}

func TestConfigExplainRedactsAndNamesTheSource(t *testing.T) {
	clearEnvNames(t, "FLEET_SMTP_PASSWORD", "FLEET_MAX_COST_USD", "CHAT_MAX_COST_USD", "CUTLASS_MAX_COST_USD")
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("FLEET_SMTP_PASSWORD=placeholder-smtp\nCHAT_MAX_COST_USD=9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if rc := configExplain([]string{"FLEET_SMTP_PASSWORD", "--env-file", path, "FLEET_MAX_COST_USD", "--json"}); rc != 0 {
			t.Errorf("rc = %d", rc)
		}
	})
	if strings.Contains(out, "placeholder-smtp") {
		t.Fatal("explain printed a secret value")
	}
	var doc struct {
		Settings []struct {
			Key, Value, Source, Spelling string
			Deprecated                   bool
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(doc.Settings) != 2 {
		t.Fatalf("want 2 settings, got %+v", doc.Settings)
	}
	cost := doc.Settings[1]
	if cost.Key != "FLEET_MAX_COST_USD" || cost.Value != "9" || cost.Spelling != "CHAT_MAX_COST_USD" || !cost.Deprecated || cost.Source != "env file" {
		t.Errorf("cost = %+v", cost)
	}

	table := captureStdout(t, func() { configExplain([]string{"--env-file", path, "FLEET_MAX_COST_USD"}) })
	if !strings.Contains(table, "env file as CHAT_MAX_COST_USD (DEPRECATED alias") {
		t.Errorf("table does not flag the deprecated spelling:\n%s", table)
	}
}

// LEGACY-ENV-ALIAS: remove after migrate-env (the two tests below).

func TestConfigMigrateEnvDryRunThenWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.env")
	orig := "# keep me\nCHAT_SERVER_TOKEN=placeholder-token\nFLEET_DATA_DIR=/var/lib/fleet\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	dry := captureStdout(t, func() {
		if rc := configMigrateEnv([]string{"--env-file", path}); rc != 0 {
			t.Errorf("dry run rc = %d", rc)
		}
	})
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Fatal("the dry run changed the file")
	}
	if !strings.Contains(dry, "- CHAT_SERVER_TOKEN=…") || !strings.Contains(dry, "+ FLEET_SERVER_TOKEN=…") || strings.Contains(dry, "placeholder-token") {
		t.Fatalf("dry run output:\n%s", dry)
	}
	wrote := captureStdout(t, func() {
		if rc := configMigrateEnv([]string{"--env-file", path, "--write"}); rc != 0 {
			t.Errorf("write rc = %d", rc)
		}
	})
	if strings.Contains(wrote, "placeholder-token") {
		t.Fatal("the write printed a secret value")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "# keep me\nFLEET_SERVER_TOKEN=placeholder-token\nFLEET_DATA_DIR=/var/lib/fleet\n" {
		t.Fatalf("rewritten file:\n%s", b)
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("want one backup, got %v", backups)
	}
	if bb, _ := os.ReadFile(backups[0]); string(bb) != orig {
		t.Error("the backup does not hold the original")
	}
}

func TestDoctorWarnsAboutDeprecatedAliases(t *testing.T) {
	clearEnvNames(t, "CHAT_MAX_ITERATIONS", "CUTLASS_TEMPERATURE")
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("CHAT_MAX_ITERATIONS=5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", path)
	t.Setenv("CUTLASS_TEMPERATURE", "0.4")
	var buf bytes.Buffer
	warnDeprecatedEnvAliases(&buf)
	out := buf.String()
	for _, want := range []string{
		"fleet config migrate-env --write",
		"CHAT_MAX_ITERATIONS is a deprecated alias (set in the env file); use FLEET_MAX_ITERATIONS",
		"CUTLASS_TEMPERATURE is a deprecated alias (set in the process env); use FLEET_TEMPERATURE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	clearEnvNames(t, "CUTLASS_TEMPERATURE")
	if err := os.WriteFile(path, []byte("FLEET_MAX_ITERATIONS=5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	warnDeprecatedEnvAliases(&buf)
	if buf.Len() != 0 {
		t.Errorf("a clean box must print nothing, got:\n%s", buf.String())
	}
}
