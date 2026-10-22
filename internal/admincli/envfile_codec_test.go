// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// envCodecCases — every value class one of fleet.env's readers treats
// specially: ` #` (fleet's parser cuts it), edge whitespace (all readers trim
// it), quotes, backslashes (systemd unescapes them), `$` and backticks.
var envCodecCases = []string{
	"plain-value-abc123",
	"",
	"abc # def",
	"foo\t#bar",
	"#leading-hash",
	"abc#def",
	"  padded  ",
	"\tpadded\t",
	"Sign in with Example",
	`"hi"`,
	`'hi'`,
	"it's a secret",
	`'`,
	`a\b`,
	`abc\`,
	`a\"b`,
	"pa$$word",
	"${HOME}/x",
	"a`id`b",
	"it's $5",
	"a=b=c",
	"pässwörd",
	"postgres://u:p%40ss@127.0.0.1:5432/chat?sslmode=disable&x=1",
}

// envCodecShell is a bash prelude that defines the scripts' real helpers:
// lib/envfile.sh, update.sh's upsert_env_file and env_get.
func envCodecShell(t *testing.T) string {
	t.Helper()
	root := repoRootFromTest(t)
	update, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	defs := make([]string, 0, 2)
	for _, fn := range []string{"upsert_env_file", "env_get"} {
		def := regexp.MustCompile(`(?ms)^` + fn + `\(\) \{.*?^\}`).Find(update)
		if def == nil {
			t.Fatalf("update.sh has no %s", fn)
		}
		defs = append(defs, string(def))
	}
	return "ok() { :; }; warn() { echo \"WARN $*\" >&2; }; info() { :; }\n" +
		". " + filepath.Join(root, "scripts", "lib", "envfile.sh") + "\n" +
		strings.Join(defs, "\n") + "\n"
}

func runEnvShell(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", script, "envtest"}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), &shellError{err: err, stderr: stderr.String()}
	}
	return string(out), nil
}

type shellError struct {
	err    error
	stderr string
}

func (e *shellError) Error() string { return e.err.Error() + ": " + e.stderr }

// TestEnvFileShellWriterRoundTrips — what bootstrap.sh/update.sh write
// (upsert_env_file → lib/envfile.sh) reads back as the exact bytes under every
// reader of fleet.env: fleet's own parser (creds.ReadEnvValues, identical to
// config.loadEnvFile), systemd's EnvironmentFile= (lib/envfile.sh's
// fleet_env_decode, checked against systemd vectors below) and the scripts'
// env_get. The on-disk form is also byte-identical to the Go writer's
// (creds.SetEnvKey), so the two writers cannot drift.
func TestEnvFileShellWriterRoundTrips(t *testing.T) {
	prelude := envCodecShell(t)
	for _, v := range envCodecCases {
		t.Run(v, func(t *testing.T) {
			dir := t.TempDir()
			shellFile := filepath.Join(dir, "shell.env")
			if err := os.WriteFile(shellFile, []byte("# kept\nOTHER=1\nK=stale\nK=stale-dup\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := runEnvShell(t, prelude+`upsert_env_file "$1" K "$2"`, shellFile, v); err != nil {
				t.Fatalf("upsert_env_file: %v", err)
			}
			vals, err := creds.ReadEnvValues(shellFile)
			if err != nil {
				t.Fatal(err)
			}
			if vals["K"] != v || vals["OTHER"] != "1" {
				t.Fatalf("fleet parser read K=%q OTHER=%q, want %q and 1", vals["K"], vals["OTHER"], v)
			}
			raw, err := os.ReadFile(shellFile)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(raw), "\nK="); n != 1 || !strings.HasPrefix(string(raw), "# kept\n") {
				t.Fatalf("want the comment kept and exactly one K= line, got:\n%s", raw)
			}
			systemd, err := runEnvShell(t, prelude+`fleet_env_get_systemd "$1" K`, shellFile)
			if err != nil || systemd != v {
				t.Fatalf("systemd reads %q (%v), want %q", systemd, err, v)
			}
			got, err := runEnvShell(t, prelude+`env_get K "$1"`, shellFile)
			if err != nil || strings.TrimSuffix(got, "\n") != v {
				t.Fatalf("env_get reads %q (%v), want %q", got, err, v)
			}

			goFile := filepath.Join(dir, "go.env")
			if err := creds.SetEnvKey(goFile, "K", v); err != nil {
				t.Fatalf("SetEnvKey: %v", err)
			}
			goRaw, err := os.ReadFile(goFile)
			if err != nil {
				t.Fatal(err)
			}
			shellLine := regexp.MustCompile(`(?m)^K=.*$`).FindString(string(raw))
			if goLine := strings.TrimSuffix(string(goRaw), "\n"); goLine != shellLine {
				t.Fatalf("writers disagree: Go wrote %q, shell wrote %q", goLine, shellLine)
			}
		})
	}
}

// TestEnvFileShellWriterRefusesUnrepresentable — a ' together with a " or \
// (or a line break) has no form systemd and fleet's parser both read back;
// upsert_env_file refuses it, warns, and leaves the file alone. The Go writer
// refuses the same values.
func TestEnvFileShellWriterRefusesUnrepresentable(t *testing.T) {
	prelude := envCodecShell(t)
	for _, v := range []string{`'starts"`, `it's "quoted"`, `it's a\b`, "two\nlines"} {
		file := filepath.Join(t.TempDir(), "fleet.env")
		if err := os.WriteFile(file, []byte("K=keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := runEnvShell(t, prelude+`upsert_env_file "$1" K "$2"`, file, v)
		if err == nil || !strings.Contains(err.Error(), "WARN could not write K") {
			t.Errorf("upsert_env_file(%q) = %v, want a refusal with a warning", v, err)
		}
		if raw, _ := os.ReadFile(file); string(raw) != "K=keep\n" {
			t.Errorf("refused write of %q changed the file: %q", v, raw)
		}
		if err := creds.SetEnvKey(filepath.Join(t.TempDir(), "go.env"), "K", v); err == nil {
			t.Errorf("SetEnvKey(%q) succeeded; the Go writer must refuse what the shell writer refuses", v)
		}
	}
}

// TestEnvFileSystemdDecoder pins fleet_env_decode to systemd's EnvironmentFile=
// semantics (src/basic/env-file.c): the vectors are what systemd hands a unit.
func TestEnvFileSystemdDecoder(t *testing.T) {
	prelude := envCodecShell(t)
	for raw, want := range map[string]string{
		`plain`:                    "plain",
		`  lead and trail  `:       "lead and trail",
		`abc # def`:                "abc # def", // no inline comments in systemd
		`"abc # def"`:              "abc # def",
		`'abc # def'`:              "abc # def",
		`"  padded  "`:             "  padded  ",
		`'a\b'`:                    `a\b`,
		`a\b`:                      "ab",
		`a\ b  `:                   "a b",
		`"a\"b\\c\$d\` + "`" + `"`: "a\"b\\c$d`",
		`"a\xb"`:                   `a\xb`,
		`'it'"'"'s'`:               "it's",
		`"it's"`:                   "it's",
		`""`:                       "",
		``:                         "",
	} {
		got, err := runEnvShell(t, prelude+`fleet_env_decode "$1"`, raw)
		if err != nil || got != want {
			t.Errorf("fleet_env_decode(%q) = %q (%v), want %q", raw, got, err, want)
		}
	}
}

// TestWebEnvMigrationKeepsQuotedValues — Codex P2 on #1838: a quoted value in
// the retired tier's fleet-web.env (systemd read it with its quotes removed)
// must reach fleet.env re-encoded, not stripped and written bare, or fleet's
// parser cuts `abc # def` at ` #` and trims edge spaces — an upgrade would
// silently change the OIDC client secret and break sign-in.
func TestWebEnvMigrationKeepsQuotedValues(t *testing.T) {
	root := repoRootFromTest(t)
	dir := t.TempDir()
	oldEnv := filepath.Join(dir, "fleet-web.env")
	newEnv := filepath.Join(dir, "fleet.env")
	old := strings.Join([]string{
		`FLEET_OIDC_CLIENT_SECRET="abc # def"`,
		`APP_SESSION_SECRET="  edge spaces  "`,
		`AUTH_SIGNING_PUBKEY='single # quoted'`,
		`FLEET_OIDC_BUTTON_LABEL="Sign in with \"Example\""`,
		`FLEET_OIDC_SCOPES=openid email profile`,
		`FLEET_OIDC_CLIENT_ID=a\b`,
		`NEXT_PUBLIC_PUBLIC_ORIGIN="https://fleet.example.com"`,
	}, "\n") + "\n"
	if err := os.WriteFile(oldEnv, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	script := envCodecShell(t) +
		". " + filepath.Join(root, "scripts", "lib", "web-tier.sh") + "\n" +
		`fleet_migrate_web_env "$1" "$2"` + "\n"
	if out, err := runEnvShell(t, script, oldEnv, newEnv); err != nil {
		t.Fatalf("migration: %v\n%s", err, out)
	}
	// What systemd handed fleet-web, which is what the operator configured.
	want := map[string]string{
		"FLEET_OIDC_CLIENT_SECRET": "abc # def",
		"APP_SESSION_SECRET":       "  edge spaces  ",
		"AUTH_SIGNING_PUBKEY":      "single # quoted",
		"FLEET_OIDC_BUTTON_LABEL":  `Sign in with "Example"`,
		"FLEET_OIDC_SCOPES":        "openid email profile",
		"FLEET_OIDC_CLIENT_ID":     "ab",
		"FLEET_PUBLIC_BASE_URL":    "https://fleet.example.com",
	}
	vals, err := creds.ReadEnvValues(newEnv)
	if err != nil {
		t.Fatal(err)
	}
	prelude := envCodecShell(t)
	for k, v := range want {
		if vals[k] != v {
			t.Errorf("fleet parser: %s = %q, want %q", k, vals[k], v)
		}
		if got, err := runEnvShell(t, prelude+`fleet_env_get_systemd "$1" "$2"`, newEnv, k); err != nil || got != v {
			t.Errorf("systemd: %s = %q (%v), want %q", k, got, err, v)
		}
	}
	if len(vals) != len(want) {
		t.Errorf("fleet.env holds %d keys, want %d:\n%v", len(vals), len(want), vals)
	}
}
