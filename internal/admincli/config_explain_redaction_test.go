package admincli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// captureLargeStdout is captureStdout with the pipe drained concurrently, so
// output larger than the pipe buffer (`explain --all`) cannot deadlock.
func captureLargeStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	return <-done
}

// withUnitDir points the systemd unit lookup at a fixture directory holding
// a fleet.service that loads envFile with EnvironmentFile= (exported=true),
// or at an empty one.
func withUnitDir(t *testing.T, envFile string, exported bool) {
	t.Helper()
	dir := t.TempDir()
	if exported {
		unit := "[Service]\nEnvironmentFile=-" + envFile + "\nExecStart=/usr/local/bin/fleet serve\n"
		if err := os.WriteFile(filepath.Join(dir, "fleet.service"), []byte(unit), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig, origCtl := systemdUnitDirs, systemctlEnvironmentFiles
	systemdUnitDirs = []string{dir}
	systemctlEnvironmentFiles = func(string) ([]string, bool) { return nil, false } // no systemctl: parse the fixture
	t.Cleanup(func() { systemdUnitDirs, systemctlEnvironmentFiles = orig, origCtl })
	t.Setenv("FLEET_SERVICE_NAME", "fleet")
}

// Codex P1 (#2057): whatever credential-shaped value an operator puts in ANY
// setting, `fleet config explain` must not print its password, token or key —
// in the table or in --json — and must not print a secret at all.
func TestConfigExplainNeverPrintsACredentialTextOrJSON(t *testing.T) {
	const (
		pw    = "pwSENTINEL9"
		tok   = "tokSENTINEL9"
		value = "https://user:" + pw + "@hostsentinel9.example/db?sslmode=disable&access_token=" + tok + "&api_key=" + tok
	)
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withUnitDir(t, path, false)
	for _, e := range envreg.All() {
		t.Setenv(e.Name, value)
	}
	t.Setenv("FLEET_SERVICE_NAME", "fleet")
	// OPENAI_CUSTOM_HEADERS, the original finding, with a header-shaped value.
	t.Setenv("OPENAI_CUSTOM_HEADERS", "Authorization: Bearer "+tok)

	text := captureLargeStdout(t, func() { configExplain([]string{"--all", "--env-file", path}) })
	js := captureLargeStdout(t, func() { configExplain([]string{"--all", "--json", "--env-file", path}) })
	for name, out := range map[string]string{"text": text, "json": js} {
		if strings.Contains(out, pw) || strings.Contains(out, tok) {
			t.Errorf("%s output leaks a credential", name)
		}
	}
	var doc struct {
		Settings []struct{ Key, Value string }
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, s := range doc.Settings {
		if s.Key == "OPENAI_CUSTOM_HEADERS" && s.Value != "<redacted>" {
			t.Errorf("OPENAI_CUSTOM_HEADERS = %q, want <redacted>", s.Value)
		}
	}
}

// Codex P2 (#2057): with the standard systemd unit (EnvironmentFile=) every
// env-file line is effective, so explain and doctor must count a spelling
// fleet's own loader would not admit; without the unit they must not.
func TestConfigExplainAndDoctorFollowSystemdExport(t *testing.T) {
	clearEnvNames(t, "FLEET_MAX_ITERATIONS", "CHAT_MAX_ITERATIONS", "CUTLASS_MAX_ITERATIONS")
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("CUTLASS_MAX_ITERATIONS=70\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", path)

	for _, exported := range []bool{false, true} {
		withUnitDir(t, path, exported)
		out := captureStdout(t, func() { configExplain([]string{"FLEET_MAX_ITERATIONS", "--json", "--env-file", path}) })
		var doc struct {
			Exported bool `json:"env_file_exported_by_systemd"`
			Settings []struct {
				Value string
				Set   bool
			}
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if doc.Exported != exported || doc.Settings[0].Set != exported {
			t.Errorf("exported=%v: explain reported exported=%v set=%v", exported, doc.Exported, doc.Settings[0].Set)
		}
		var buf bytes.Buffer
		warnDeprecatedEnvAliases(&buf)
		want := "is ignored today"
		if exported {
			want = "reaches the server only through the systemd unit's EnvironmentFile="
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("exported=%v: doctor output lacks %q:\n%s", exported, want, buf.String())
		}
	}
}

func TestConfigMigrateEnvRenamesLoaderIgnoredAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("CUTLASS_MAX_ITERATIONS=70\nCHAT_TLS_MODE=auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if rc := configMigrateEnv([]string{"--env-file", path, "--write"}); rc != 0 {
			t.Errorf("rc = %d", rc)
		}
	})
	if b, _ := os.ReadFile(path); string(b) != "FLEET_MAX_ITERATIONS=70\nCHAT_TLS_MODE=auto\n" {
		t.Fatalf("rewritten file:\n%s", b)
	}
	for _, want := range []string{
		"effective only through the systemd unit's EnvironmentFile=",
		"CHAT_TLS_MODE left as is: only the operator CLI's local health probe",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
}
