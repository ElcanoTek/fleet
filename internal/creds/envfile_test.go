package creds

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadEnvValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.env")
	contents := "" +
		"# a comment line\n" +
		"\n" +
		"FLEET_SERVER_TOKEN=plaintok\n" +
		"CHAT_SERVER_TOKEN=\"quotedtok\"\n" +
		"export FLEET_SERVER_ADDR=127.0.0.1:8080\n" +
		"OPENROUTER_API_KEY=sk-xyz # inline comment\n" +
		"EMPTY=\n" +
		"noeq line without equals\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("selected keys, mirroring the server's value handling", func(t *testing.T) {
		vals, err := ReadEnvValues(path, "FLEET_SERVER_TOKEN", "CHAT_SERVER_TOKEN", "FLEET_SERVER_ADDR", "OPENROUTER_API_KEY", "ABSENT")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"FLEET_SERVER_TOKEN": "plaintok",       // plain
			"CHAT_SERVER_TOKEN":  "quotedtok",      // surrounding quotes stripped
			"FLEET_SERVER_ADDR":  "127.0.0.1:8080", // export prefix tolerated
			"OPENROUTER_API_KEY": "sk-xyz",         // inline comment trimmed
		}
		for k, v := range want {
			if vals[k] != v {
				t.Errorf("%s = %q, want %q", k, vals[k], v)
			}
		}
		if _, ok := vals["ABSENT"]; ok {
			t.Errorf("ABSENT should not appear: %v", vals)
		}
	})

	t.Run("no keys requested reads every assignment", func(t *testing.T) {
		vals, err := ReadEnvValues(path)
		if err != nil {
			t.Fatal(err)
		}
		// EMPTY= is a valid (empty-value) assignment; the no-equals line is not.
		if _, ok := vals["EMPTY"]; !ok {
			t.Errorf("EMPTY should be present with empty value, got %v", vals)
		}
		if _, ok := vals["noeq line without equals"]; ok {
			t.Errorf("a line without '=' must be skipped, got %v", vals)
		}
		if len(vals) != 5 {
			t.Errorf("want 5 assignments parsed, got %d: %v", len(vals), vals)
		}
	})

	t.Run("missing file yields empty map and no error", func(t *testing.T) {
		vals, err := ReadEnvValues(filepath.Join(dir, "nope.env"), "FLEET_SERVER_TOKEN")
		if err != nil {
			t.Fatalf("missing file should not error, got %v", err)
		}
		if len(vals) != 0 {
			t.Errorf("missing file should yield empty map, got %v", vals)
		}
	})

	t.Run("unreadable path returns the error (caller falls back)", func(t *testing.T) {
		// A directory is openable but unreadable as lines — exercises the error path.
		if _, err := ReadEnvValues(dir, "FLEET_SERVER_TOKEN"); err == nil {
			t.Errorf("reading a directory should return an error")
		}
	})
}
