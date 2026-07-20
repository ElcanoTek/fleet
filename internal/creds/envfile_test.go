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

// #834: SetEnvKey must write values so the shared parser reads back the exact
// bytes it was given. Before the encode step, literal quotes / edge whitespace
// / inline-comment sequences silently mangled on the next read, and a newline
// physically corrupted the file.
func TestSetEnvKeyRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"plain api key", "plain-value-abc123"},
		{"empty", ""},
		{"literal surrounding double quotes", `"hi"`},
		{"literal surrounding single quotes", `'hi'`},
		{"interior quote", "it's a secret"},
		{"leading and trailing spaces", "  padded  "},
		{"inline comment sequence", "foo #bar"},
		{"tab comment sequence", "foo\t#bar"},
		{"hash without space", "abc#def"},
		{"equals signs", "a=b=c"},
		{"export lookalike", "export PATH=x"},
		{"only a quote", `'`},
		{"mixed quote edges", `'starts"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fleet.env")
			if err := SetEnvKey(path, "ROUNDTRIP_KEY", tc.value); err != nil {
				t.Fatalf("SetEnvKey: %v", err)
			}
			vals, err := ReadEnvValues(path, "ROUNDTRIP_KEY")
			if err != nil {
				t.Fatalf("ReadEnvValues: %v", err)
			}
			if got := vals["ROUNDTRIP_KEY"]; got != tc.value {
				t.Fatalf("round-trip = %q, want %q", got, tc.value)
			}
		})
	}
}

// A plain value must stay byte-verbatim on disk — no quote churn for the
// common API-key case (other consumers of the file may read quotes literally).
func TestSetEnvKeyPlainValueStaysVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := SetEnvKey(path, "K", "plain-value-123"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "K=plain-value-123\n" {
		t.Fatalf("on-disk line = %q, want unquoted verbatim", string(raw))
	}
}

// A value with a line break cannot be represented in a line-oriented env file;
// writing it raw would corrupt the file (the tail parses as a bogus key). It
// must be refused, and the file left untouched.
func TestSetEnvKeyRefusesLineBreaks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := SetEnvKey(path, "GOOD", "keep-me"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"multi\nline", "carriage\rreturn"} {
		if err := SetEnvKey(path, "BAD", v); err == nil {
			t.Fatalf("SetEnvKey(%q) succeeded, want refusal", v)
		}
	}
	vals, err := ReadEnvValues(path)
	if err != nil {
		t.Fatal(err)
	}
	if vals["GOOD"] != "keep-me" || len(vals) != 1 {
		t.Fatalf("file mutated by refused writes: %v", vals)
	}
}
