package admincli

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// writeTestEnvFile writes an env file into a temp dir and returns its path.
func writeTestEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return path
}

func TestRedactEnvValue(t *testing.T) {
	cases := []struct {
		name, key, value, want string
	}{
		{"secret key name masks whole value", "OPENROUTER_API_KEY", "sk-or-abc123", "[REDACTED]"},
		{"token name masks", "GITHUB_TOKEN", "ghp_zzz", "[REDACTED]"},
		{"password name masks", "SMTP_PASSWORD", "hunter2", "[REDACTED]"},
		{"credential name masks", "GOOGLE_CREDENTIAL", "blob", "[REDACTED]"},
		{"plain knob passes through", "FLEET_MAX_COST_USD", "5.0", "5.0"},
		{"dsn userinfo is stripped", "FLEET_CHAT_DATABASE_URL", "postgres://chat:s3cret@127.0.0.1:5432/chat", "postgres://***@127.0.0.1:5432/chat"},
		{"plain url untouched", "FLEET_PUBLIC_URL", "https://fleet.example.com", "https://fleet.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactEnvValue(tc.key, tc.value); got != tc.want {
				t.Fatalf("redactEnvValue(%s, %s) = %q, want %q", tc.key, tc.value, got, tc.want)
			}
		})
	}
}

// TestPrintRedactedEnvFile_NeverPrintsSecrets is the load-bearing test: no
// secret VALUE from the file may reach the rendered output, whatever else the
// renderer does.
func TestPrintRedactedEnvFile_NeverPrintsSecrets(t *testing.T) {
	path := writeTestEnvFile(t, strings.Join([]string{
		"# provisioning comment",
		"OPENROUTER_API_KEY=sk-or-verysecret",
		"FLEET_SHARED_TOKEN='tok-3nvfile'",
		"FLEET_CHAT_DATABASE_URL=postgres://chat:dbpass@127.0.0.1:5432/chat?sslmode=disable",
		"FLEET_MAX_COST_USD=2.5",
		"",
	}, "\n"))
	var sb strings.Builder
	if err := printRedactedEnvFile(&sb, path); err != nil {
		t.Fatalf("printRedactedEnvFile: %v", err)
	}
	out := sb.String()
	for _, secret := range []string{"sk-or-verysecret", "tok-3nvfile", "dbpass"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks secret %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{
		"# " + path,
		"OPENROUTER_API_KEY=[REDACTED]",
		"FLEET_SHARED_TOKEN=[REDACTED]",
		"FLEET_CHAT_DATABASE_URL=postgres://***@127.0.0.1:5432/chat?sslmode=disable",
		"FLEET_MAX_COST_USD=2.5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintRedactedEnvFile_MissingFileIsANote(t *testing.T) {
	var sb strings.Builder
	if err := printRedactedEnvFile(&sb, filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if !strings.Contains(sb.String(), "not present") {
		t.Fatalf("want a not-present note, got %q", sb.String())
	}
}

func TestResolveEditorArgv(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	t.Run("flag wins over env", func(t *testing.T) {
		got := resolveEditorArgv("hx", env(map[string]string{"VISUAL": "vim", "EDITOR": "nano"}))
		if len(got) != 1 || got[0] != "hx" {
			t.Fatalf("got %v, want [hx]", got)
		}
	})
	t.Run("VISUAL wins over EDITOR", func(t *testing.T) {
		got := resolveEditorArgv("", env(map[string]string{"VISUAL": "vim", "EDITOR": "nano"}))
		if len(got) != 1 || got[0] != "vim" {
			t.Fatalf("got %v, want [vim]", got)
		}
	})
	t.Run("EDITOR with args splits", func(t *testing.T) {
		got := resolveEditorArgv("", env(map[string]string{"EDITOR": "code -w"}))
		if len(got) != 2 || got[0] != "code" || got[1] != "-w" {
			t.Fatalf("got %v, want [code -w]", got)
		}
	})
	t.Run("nothing set yields nil", func(t *testing.T) {
		if got := resolveEditorArgv("  ", env(nil)); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
}

func TestParseEditorChoice(t *testing.T) {
	cases := []struct {
		answer  string
		def     int
		want    int
		wantErr bool
	}{
		{"", 1, 1, false},        // empty takes the default
		{"1", 0, 0, false},       // number
		{"vim", 0, 1, false},     // display name
		{"hx", 0, 2, false},      // binary name
		{"HELIX\n", 0, 2, false}, // case + newline tolerated
		{"7", 0, 0, true},
		{"emacs", 0, 0, true},
	}
	for _, tc := range cases {
		got, err := parseEditorChoice(tc.answer, tc.def)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseEditorChoice(%q): want error", tc.answer)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseEditorChoice(%q, %d) = %d, %v; want %d, nil", tc.answer, tc.def, got, err, tc.want)
		}
	}
}

func TestLintEnvFile(t *testing.T) {
	path := writeTestEnvFile(t, strings.Join([]string{
		"# fine: comment",
		"GOOD_KEY=value",
		"export EXPORTED_KEY=value",
		"this line is broken",
		"=nokey",
		"GOOD_KEY=shadowing-duplicate",
		"",
	}, "\n"))
	warnings := lintEnvFile(path)
	if len(warnings) != 3 {
		t.Fatalf("want 3 warnings, got %d: %v", len(warnings), warnings)
	}
	for i, wantFrag := range []string{":4: not a KEY=VALUE line", ":5: not a KEY=VALUE line", "duplicate key GOOD_KEY"} {
		if !strings.Contains(warnings[i], wantFrag) {
			t.Errorf("warnings[%d] = %q, want it to contain %q", i, warnings[i], wantFrag)
		}
	}
}

func TestLintEnvFile_CleanFileHasNoWarnings(t *testing.T) {
	path := writeTestEnvFile(t, "# comment\nA=1\nB='two'\n")
	if warnings := lintEnvFile(path); len(warnings) != 0 {
		t.Fatalf("want no warnings, got %v", warnings)
	}
}

func TestEnsureEditableEnvFile(t *testing.T) {
	t.Run("creates missing file 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "new.env")
		if err := ensureEditableEnvFile(path); err != nil {
			t.Fatalf("ensureEditableEnvFile: %v", err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 0600", fi.Mode().Perm())
		}
	})
	t.Run("creates a missing parent dir", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sub", "dir", "new.env")
		if err := ensureEditableEnvFile(path); err != nil {
			t.Fatalf("ensureEditableEnvFile: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat: %v", err)
		}
	})
	t.Run("unwritable file names the sudo fix", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can write anything")
		}
		path := filepath.Join(t.TempDir(), "locked.env")
		if err := os.WriteFile(path, []byte("A=1\n"), 0o400); err != nil {
			t.Fatalf("write: %v", err)
		}
		err := ensureEditableEnvFile(path)
		if err == nil || !strings.Contains(err.Error(), "sudo fleet env edit") {
			t.Fatalf("want a sudo-hint error, got %v", err)
		}
	})
}

// TestEnvEdit_TrueEditor drives the whole edit path with `true` as the editor
// (exits 0 without touching the file) — the no-change branch — and with a tiny
// script that appends a line — the saved branch.
func TestEnvEdit_TrueEditor(t *testing.T) {
	t.Run("no changes", func(t *testing.T) {
		path := writeTestEnvFile(t, "A=1\n")
		if code := envEdit([]string{"--env-file", path, "--editor", "true"}); code != 0 {
			t.Fatalf("envEdit = %d, want 0", code)
		}
	})
	t.Run("editor writes are saved and mode restored", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "fleet.env")
		if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		// A fake "editor": appends a line to its file argument and loosens the
		// mode, so the post-edit chmod has something to restore.
		script := filepath.Join(dir, "editor.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'B=2' >> \"$1\"\nchmod 644 \"$1\"\n"), 0o700); err != nil {
			t.Fatalf("write script: %v", err)
		}
		if code := envEdit([]string{"--env-file", path, "--editor", script}); code != 0 {
			t.Fatalf("envEdit = %d, want 0", code)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(b) != "A=1\nB=2\n" {
			t.Fatalf("content = %q, want the appended line", b)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 0600 restored after the editor loosened it", fi.Mode().Perm())
		}
	})
	t.Run("editor that saves via rename keeps mode and owner", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "fleet.env")
		if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		// vim's default save: write a sibling, rename over the original — a new
		// inode with the editor's umask mode and the editor's uid.
		script := filepath.Join(dir, "editor.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'A=1\\nB=2\\n' > \"$1.new\"\nchmod 644 \"$1.new\"\nmv \"$1.new\" \"$1\"\n"), 0o700); err != nil {
			t.Fatalf("write script: %v", err)
		}
		if code := envEdit([]string{"--env-file", path, "--editor", script}); code != 0 {
			t.Fatalf("envEdit = %d, want 0", code)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if after.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 0600 after a rename-save", after.Mode().Perm())
		}
		bu, bg, _ := creds.FileOwner(before)
		au, ag, _ := creds.FileOwner(after)
		if bu != au || bg != ag {
			t.Fatalf("owner changed %d:%d → %d:%d across the edit", bu, bg, au, ag)
		}
	})
	t.Run("failing editor is an error", func(t *testing.T) {
		path := writeTestEnvFile(t, "A=1\n")
		if code := envEdit([]string{"--env-file", path, "--editor", "false"}); code == 0 {
			t.Fatal("envEdit with a failing editor should be non-zero")
		}
	})
}

func TestPickEditor_ParsesChoiceFromReader(t *testing.T) {
	// "1" picks nano; whether nano is installed decides between the run path
	// and the dnf offer, so use whichever of the menu binaries IS present to
	// keep the test hermetic. Fall back to skipping when none is installed
	// (a dnf prompt would need more stdin than this test scripts).
	var choice string
	for i, e := range editorMenu {
		if _, err := exec.LookPath(e.bin); err == nil {
			choice = []string{"1", "2", "3"}[i]
			break
		}
	}
	if choice == "" {
		t.Skip("none of nano/vim/hx installed")
	}
	var out strings.Builder
	bin, err := pickEditor(bufio.NewReader(strings.NewReader(choice+"\n")), &out)
	if err != nil {
		t.Fatalf("pickEditor: %v", err)
	}
	if bin == "" {
		t.Fatal("pickEditor returned an empty binary")
	}
	if !strings.Contains(out.String(), "Pick an editor:") {
		t.Fatalf("menu not rendered: %q", out.String())
	}
}

func TestPickEditor_InvalidChoice(t *testing.T) {
	var out strings.Builder
	if _, err := pickEditor(bufio.NewReader(strings.NewReader("emacs\n")), &out); err == nil {
		t.Fatal("want error for an off-menu choice")
	}
}

func TestCmdEnv_Dispatch(t *testing.T) {
	if code := cmdEnv([]string{"bogus"}); code != 1 {
		t.Fatalf("unknown subcommand: code = %d, want 1", code)
	}
	if code := cmdEnv([]string{"help"}); code != 0 {
		t.Fatalf("help: code = %d, want 0", code)
	}
}
