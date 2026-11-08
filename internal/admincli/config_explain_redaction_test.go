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
	orig, origCtl := systemdUnitDirs, systemctlShow
	systemdUnitDirs = []string{dir}
	systemctlShow = func(string) (string, bool) { return "", false } // no systemctl: parse the fixture
	t.Cleanup(func() { systemdUnitDirs, systemctlShow = orig, origCtl })
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
		Settings []struct {
			Key   string `json:"key"`
			Value string `json:"configured_value"`
		} `json:"settings"`
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
// env-file line is effective, so explain must count a spelling fleet's own
// loader would not admit; without the unit it must not. A removed alias
// (ADR-0106) is never effective, and doctor reports it either way.
func TestConfigExplainAndDoctorFollowSystemdExport(t *testing.T) {
	clearEnvNames(t, "FLEET_IMAGE_MODEL", "FLEET_MAX_ITERATIONS", "CHAT_MAX_ITERATIONS", "CUTLASS_MAX_ITERATIONS")
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("FLEET_IMAGE_MODEL=img-model\nCUTLASS_MAX_ITERATIONS=70\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", path)

	for _, exported := range []bool{false, true} {
		withUnitDir(t, path, exported)
		out := captureStdout(t, func() {
			configExplain([]string{"FLEET_IMAGE_MODEL", "FLEET_MAX_ITERATIONS", "--json", "--env-file", path})
		})
		var doc struct {
			Sources struct {
				Unit *struct {
					Files []string `json:"environment_files"`
				} `json:"systemd_unit"`
			} `json:"sources"`
			Settings []struct {
				Value  string   `json:"configured_value"`
				Set    bool     `json:"set"`
				Source string   `json:"source"`
				Notes  []string `json:"notes"`
			} `json:"settings"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if len(doc.Settings) != 2 {
			t.Fatalf("want 2 settings, got %+v", doc.Settings)
		}
		img, iters := doc.Settings[0], doc.Settings[1]
		gotExported := doc.Sources.Unit != nil && len(doc.Sources.Unit.Files) == 1 && doc.Sources.Unit.Files[0] == path
		if gotExported != exported || img.Set != exported {
			t.Errorf("exported=%v: explain reported unit=%+v setting=%+v", exported, doc.Sources.Unit, img)
		}
		if exported && (img.Value != "img-model" || img.Source != "systemd unit") {
			t.Errorf("exported: setting %+v, want img-model from the systemd unit", img)
		}
		if iters.Set || !strings.Contains(strings.Join(iters.Notes, "\n"), "CUTLASS_MAX_ITERATIONS is set") {
			t.Errorf("exported=%v: the removed alias must stay unread and noted: %+v", exported, iters)
		}
		var buf bytes.Buffer
		warnRetiredEnvAliases(&buf)
		want := "CUTLASS_MAX_ITERATIONS is set in the env file, but fleet no longer reads it"
		if !strings.Contains(buf.String(), want) || strings.Count(buf.String(), "CUTLASS_MAX_ITERATIONS is set") != 1 {
			t.Errorf("exported=%v: doctor output must report %q once:\n%s", exported, want, buf.String())
		}
	}
}
