package creds

import (
	"os"
	"path/filepath"
	"strings"
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

// #834, #1838: SetEnvKey must write values so EVERY reader of the file reads
// back the exact bytes it was given — fleet's parser (ReadEnvValues, identical
// to config.loadEnvFile), systemd's EnvironmentFile= (what deploy/fleet.service
// injects, modelled by systemdEnvValue) and the scripts' env_get. Before the
// encode step, literal quotes / edge whitespace / inline-comment sequences
// silently mangled on the next read; before #1838 a backslash or a ' inside a
// quoted value read back differently under systemd.
func TestSetEnvKeyRoundTrip(t *testing.T) {
	for _, tc := range envRoundTripCases {
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
				t.Fatalf("fleet parser read %q, want %q", got, tc.value)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			onDisk := strings.TrimSuffix(strings.TrimPrefix(string(raw), "ROUNDTRIP_KEY="), "\n")
			if got := systemdEnvValue(onDisk); got != tc.value {
				t.Fatalf("systemd reads %q from %q, want %q", got, onDisk, tc.value)
			}
			if got := envGetValue(onDisk); got != tc.value {
				t.Fatalf("env_get reads %q from %q, want %q", got, onDisk, tc.value)
			}
		})
	}
}

// envRoundTripCases — every value class a reader treats specially. Shared with
// TestEnvEncodingMatchesShellWriter, which runs scripts/lib/envfile.sh.
var envRoundTripCases = []struct {
	name  string
	value string
}{
	{"plain api key", "plain-value-abc123"},
	{"empty", ""},
	{"literal surrounding double quotes", `"hi"`},
	{"literal surrounding single quotes", `'hi'`},
	{"interior quote", "it's a secret"},
	{"leading and trailing spaces", "  padded  "},
	{"tabs at the edges", "\tpadded\t"},
	{"inline comment sequence", "foo #bar"},
	{"oidc secret with a comment marker", "abc # def"},
	{"tab comment sequence", "foo\t#bar"},
	{"leading hash", "#abc"},
	{"hash without space", "abc#def"},
	{"equals signs", "a=b=c"},
	{"export lookalike", "export PATH=x"},
	{"only a quote", `'`},
	{"backslash", `a\b`},
	{"trailing backslash", `abc\`},
	{"backslash escape lookalike", `a\"b`},
	{"dollar", "pa$$word"},
	{"variable lookalike", "${HOME}/x"},
	{"backtick", "a`id`b"},
	{"interior spaces", "Sign in with Example"},
	{"single quote and dollar", "it's $5"},
	{"non-ascii", "pässwörd"},
}

// TestSetEnvKeyRefusesUnrepresentable — a ' together with a " or \ has no
// quoting that systemd and fleet's parser both read back unchanged (systemd's
// only way to carry it is backslash escapes, which fleet's parser keeps). The
// write is refused and the file left as it was.
func TestSetEnvKeyRefusesUnrepresentable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := SetEnvKey(path, "GOOD", "keep-me"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{`'starts"`, `it's "quoted"`, `it's a\b`} {
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

// systemdEnvValue models systemd's EnvironmentFile= value parsing
// (src/basic/env-file.c, the PRE_VALUE/VALUE/quote states) for the text after
// `KEY=`: single quotes are literal; double quotes unescape \" \\ \` \$ and
// keep any other backslash; an unquoted value unescapes \x and drops trailing
// whitespace; quoted and unquoted runs concatenate.
func systemdEnvValue(raw string) string {
	const (
		pre = iota
		val
		valEsc
		sq
		dq
		dqEsc
	)
	var out []byte
	state, ws := pre, -1
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch state {
		case pre:
			switch c {
			case '\'':
				state = sq
			case '"':
				state = dq
			case '\\':
				state = valEsc
			case ' ', '\t':
			default:
				state, ws = val, -1
				out = append(out, c)
			}
		case val:
			switch c {
			case '\\':
				state = valEsc
			case ' ', '\t':
				if ws < 0 {
					ws = len(out)
				}
				out = append(out, c)
			default:
				ws = -1
				out = append(out, c)
			}
		case valEsc:
			state, ws = val, -1
			out = append(out, c)
		case sq:
			if c == '\'' {
				state = pre
			} else {
				out = append(out, c)
			}
		case dq:
			switch c {
			case '"':
				state = pre
			case '\\':
				state = dqEsc
			default:
				out = append(out, c)
			}
		case dqEsc:
			if !strings.ContainsRune("\"\\`$", rune(c)) {
				out = append(out, '\\')
			}
			out = append(out, c)
			state = dq
		}
	}
	if state == val && ws >= 0 {
		out = out[:ws]
	}
	return string(out)
}

// envGetValue models the scripts' env_get: one leading and one trailing quote
// character stripped, nothing else.
func envGetValue(raw string) string {
	if raw != "" && (raw[0] == '"' || raw[0] == '\'') {
		raw = raw[1:]
	}
	if raw != "" && (raw[len(raw)-1] == '"' || raw[len(raw)-1] == '\'') {
		raw = raw[:len(raw)-1]
	}
	return raw
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

// TestSetEnvKeyDedupesDuplicateLines — the server's loader is last-assignment-
// wins (config.loadEnvFileFiltered calls os.Setenv per line), so an upsert that
// replaced only the FIRST of two duplicate lines left the stale later one in
// force: `fleet config set-*` printed "set KEY" and changed nothing the service
// saw. That is the duplicate-line outage class. The result must be exactly one
// line for the key, in the first occurrence's position, with everything else
// untouched.
func TestSetEnvKeyDedupesDuplicateLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "# header\nA=1\nOPENX_API_KEY=old-1\nB=2\nOPENX_API_KEY=old-2\nexport OPENX_API_KEY=old-3\nC=3\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetEnvKey(path, "OPENX_API_KEY", "new"); err != nil {
		t.Fatalf("SetEnvKey: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# header\nA=1\nOPENX_API_KEY=new\nB=2\nC=3\n"
	if string(got) != want {
		t.Errorf("after upsert:\n%s\nwant:\n%s", got, want)
	}
	vals, err := ReadEnvValues(path, "OPENX_API_KEY")
	if err != nil || vals["OPENX_API_KEY"] != "new" {
		t.Errorf("read back = %q, %v; want new", vals["OPENX_API_KEY"], err)
	}
}

// TestWriteEnvLinesKeepsModeAndOwnerForCaller — a non-root writer can only
// have written a file it owns, so the rename must leave the owner as it was
// and the mode 0600; PreserveOwner must be a no-op (never an error) there.
func TestWriteEnvLinesKeepsModeAndOwnerForCaller(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetEnvKey(path, "B", "2"); err != nil {
		t.Fatalf("SetEnvKey: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", after.Mode().Perm())
	}
	bu, bg, bok := FileOwner(before)
	au, ag, aok := FileOwner(after)
	if bok != aok || bu != au || bg != ag {
		t.Errorf("owner changed across the rewrite: %d:%d → %d:%d", bu, bg, au, ag)
	}
	if err := PreserveOwner(path, before); err != nil {
		t.Errorf("PreserveOwner no-op returned %v", err)
	}
	if err := PreserveOwner(path, nil); err != nil {
		t.Errorf("PreserveOwner(nil) returned %v", err)
	}
}
