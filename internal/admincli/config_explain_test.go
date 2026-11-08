package admincli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr is captureStdout for os.Stderr (where errf writes).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()
	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return buf.String()
}

func clearEnvNames(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		t.Setenv(n, "")
		_ = os.Unsetenv(n)
	}
}

func TestConfigExplainRedactsAndNamesTheSource(t *testing.T) {
	stubSystemd(t, t.TempDir(), "", false) // hermetic: no systemd unit on the test box
	clearEnvNames(t, "FLEET_SMTP_PASSWORD", "FLEET_MAX_COST_USD", "CHAT_MAX_COST_USD", "CUTLASS_MAX_COST_USD")
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("FLEET_SMTP_PASSWORD=placeholder-smtp\nFLEET_MAX_COST_USD=9\n"), 0o600); err != nil {
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
			Key      string `json:"key"`
			Value    string `json:"configured_value"`
			Source   string `json:"source"`
			Spelling string `json:"spelling"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(doc.Settings) != 2 {
		t.Fatalf("want 2 settings, got %+v", doc.Settings)
	}
	cost := doc.Settings[1]
	if cost.Key != "FLEET_MAX_COST_USD" || cost.Value != "9" || cost.Spelling != "FLEET_MAX_COST_USD" || cost.Source != "env file" {
		t.Errorf("cost = %+v", cost)
	}
}

// ADR-0106: a removed CHAT_/CUTLASS_ spelling is not read. explain notes it
// on the canonical setting (which keeps its default) and refuses it as a key,
// naming the FLEET_ one; set-env refuses to write it.
func TestConfigExplainNamesTheCanonicalKeyForARemovedAlias(t *testing.T) {
	stubSystemd(t, t.TempDir(), "", false)
	clearEnvNames(t, "FLEET_MAX_COST_USD", "CHAT_MAX_COST_USD", "CUTLASS_MAX_COST_USD")
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("CHAT_MAX_COST_USD=9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	table := captureStdout(t, func() { configExplain([]string{"--env-file", path, "FLEET_MAX_COST_USD"}) })
	if !strings.Contains(table, "CHAT_MAX_COST_USD is set (env file), but it is a removed alias (ADR-0106) and nothing reads it: rename it to FLEET_MAX_COST_USD") {
		t.Errorf("explain does not flag the removed spelling:\n%s", table)
	}
	if strings.Contains(table, " 9 ") {
		t.Errorf("explain took the removed spelling's value:\n%s", table)
	}
	stderr := captureStderr(t, func() {
		if rc := configExplain([]string{"--env-file", path, "CHAT_MAX_COST_USD"}); rc == 0 {
			t.Error("explain of a removed alias must fail")
		}
	})
	if !strings.Contains(stderr, "a removed alias of FLEET_MAX_COST_USD") {
		t.Errorf("explain error does not name the canonical key:\n%s", stderr)
	}
	stderr = captureStderr(t, func() {
		if rc := configSetEnv([]string{"CHAT_MAX_COST_USD", "--value", "9", "--env-file", path}); rc == 0 {
			t.Error("set-env of a removed alias must fail")
		}
	})
	if !strings.Contains(stderr, "set FLEET_MAX_COST_USD instead") {
		t.Errorf("set-env error does not name the canonical key:\n%s", stderr)
	}
}

func TestDoctorWarnsAboutRemovedAliases(t *testing.T) {
	stubSystemd(t, t.TempDir(), "", false) // hermetic: no systemd unit on the test box
	clearEnvNames(t, "CHAT_MAX_ITERATIONS", "CUTLASS_TEMPERATURE", "DATA_DIR")
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte("CHAT_MAX_ITERATIONS=5\nCHAT_RATE_PER_MIN=10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", path)
	t.Setenv("CUTLASS_TEMPERATURE", "0.4")
	t.Setenv("DATA_DIR", "/srv/data")
	var buf bytes.Buffer
	warnRetiredEnvAliases(&buf)
	out := buf.String()
	for _, want := range []string{
		"WARN removed CHAT_/CUTLASS_ env aliases are set (ADR-0106)",
		"CHAT_MAX_ITERATIONS is set in the env file, but fleet no longer reads it (the CHAT_/CUTLASS_ aliases were removed, ADR-0106): rename it to FLEET_MAX_ITERATIONS",
		"CUTLASS_TEMPERATURE is set in the process env, but fleet no longer reads it",
		"DATA_DIR is set in the process env, but fleet no longer reads it (the CHAT_/CUTLASS_ aliases were removed, ADR-0106): rename it to FLEET_DATA_DIR",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CHAT_RATE_PER_MIN") || strings.Contains(out, "0.4") || strings.Contains(out, "/srv/data") {
		t.Errorf("doctor flagged a canonical CHAT_ setting or printed a value:\n%s", out)
	}
	clearEnvNames(t, "CUTLASS_TEMPERATURE", "DATA_DIR")
	if err := os.WriteFile(path, []byte("FLEET_MAX_ITERATIONS=5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	warnRetiredEnvAliases(&buf)
	if buf.Len() != 0 {
		t.Errorf("a clean box must print nothing, got:\n%s", buf.String())
	}
}

// ADR-0106: fleet doctor also reports a ${CHAT_X} reference in the client
// bundle, naming the FLEET_ key, without loading the bundle.
func TestDoctorWarnsAboutRemovedManifestRefs(t *testing.T) {
	stubSystemd(t, t.TempDir(), "", false)
	dir := t.TempDir()
	manifest := "mcp_servers:\n  - name: x\n    command: \"python3${CHAT_TLS_MODE:?x}\"\n    env:\n      CUTLASS_ALLOWED_DIRS: \"${CUTLASS_ALLOWED_DIRS}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
	t.Setenv("FLEET_ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
	var buf bytes.Buffer
	warnRetiredEnvAliases(&buf)
	out := buf.String()
	if !strings.Contains(out, "${CHAT_TLS_MODE}, a removed alias of FLEET_TLS_MODE") || strings.Contains(out, "${CUTLASS_ALLOWED_DIRS}") {
		t.Errorf("doctor output:\n%s", out)
	}
}
