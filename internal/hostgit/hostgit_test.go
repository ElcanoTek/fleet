package hostgit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRepo creates a repository on branch main with one commit (a.txt) and
// returns its path. It runs plain git on purpose: the fixture is built the
// way a person would.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "t@t.t")
	run(t, dir, "config", "user.name", "t")
	write(t, filepath.Join(dir, "a.txt"), "hello\n", 0o644)
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// markerScript writes a script that appends its arguments to a marker file
// outside the repository and returns both paths. It never reads stdin, so a
// long-running process filter git did start cannot hang the test.
func markerScript(t *testing.T) (script, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "ran")
	script = filepath.Join(dir, "mark.sh")
	write(t, script, "#!/bin/sh\necho \"$*\" >> '"+marker+"'\nexit 0\n", 0o755)
	return script, marker
}

func assertNotRun(t *testing.T, marker string) {
	t.Helper()
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a program named by the repository ran: %q", data)
	}
}

// TestOperatorCheckoutKeepsCredentialsAndTransports: in the operator's own
// checkout, fleet keeps the credential helpers, ssh command and transports the
// operator configured — a fetch from a local-path remote works and no
// credential override is passed — while hooks stay off.
func TestOperatorCheckoutKeepsCredentialsAndTransports(t *testing.T) {
	upstream := fixtureRepo(t)
	repo := fixtureRepo(t)
	script, marker := markerScript(t)
	run(t, repo, "remote", "add", "origin", upstream)
	write(t, filepath.Join(repo, ".git", "hooks", "reference-transaction"), "#!/bin/sh\nexec "+script+" hook\n", 0o755)
	cmd, err := command(context.Background(), repo, "fetch", "--quiet", "origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cmd.Args {
		if strings.HasPrefix(a, "credential.") || strings.HasPrefix(a, "core.sshCommand") || strings.HasPrefix(a, "core.askPass") {
			t.Errorf("operator command overrides %q", a)
		}
	}
	for _, kv := range cmd.Env {
		// Whatever askpass the operator's environment names is kept as is;
		// only an emptied one or a protocol allowlist would be fleet's doing.
		if strings.HasPrefix(kv, "GIT_ALLOW_PROTOCOL=") || kv == "GIT_ASKPASS=" || kv == "GIT_TERMINAL_PROMPT=0" {
			t.Errorf("operator command sets %q", kv)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fetch from a local-path remote: %v\n%s", err, out)
	}
	assertNotRun(t, marker)
}

// TestCommandActsOnDirWhateverTheEnvironment: GIT_DIR, GIT_WORK_TREE and
// GIT_CONFIG in fleet's environment do not redirect a command away from dir.
func TestCommandActsOnDirWhateverTheEnvironment(t *testing.T) {
	repo := fixtureRepo(t)
	other := fixtureRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_CONFIG", filepath.Join(t.TempDir(), "nothing"))
	out, err := Output(context.Background(), repo, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if got != want {
		t.Fatalf("toplevel = %q, want %q", got, want)
	}
}

// TestOperatorCommandsAreBounded: every command has a WaitDelay, so a child
// that keeps the output pipe open cannot outlive its context, and Output's
// own bound applies whatever deadline the caller passes.
func TestOperatorCommandsAreBounded(t *testing.T) {
	repo := fixtureRepo(t)
	cmd, err := command(context.Background(), repo, "status")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.WaitDelay <= 0 {
		t.Fatalf("WaitDelay = %v, want a bound", cmd.WaitDelay)
	}
	if MaxDuration <= 0 || MaxDuration > 5*time.Minute {
		t.Fatalf("MaxDuration = %v", MaxDuration)
	}
}

// TestCommandRunsOnlyFleetsSubcommands: command builds only the subcommands
// fleet's checks use, and needs a directory.
func TestCommandRunsOnlyFleetsSubcommands(t *testing.T) {
	repo := fixtureRepo(t)
	for _, args := range [][]string{{"worktree", "add", "x"}, {"commit"}, {"checkout", "main"}, {"config", "x.y", "z"}, {}} {
		if _, err := command(context.Background(), repo, args...); !errors.Is(err, ErrRefused) {
			t.Errorf("Command %v: err = %v, want ErrRefused", args, err)
		}
	}
	for _, sub := range []string{"rev-parse", "rev-list", "symbolic-ref", "status", "fetch"} {
		if _, err := command(context.Background(), repo, sub); err != nil {
			t.Errorf("Command %s: %v", sub, err)
		}
	}
	if _, err := command(context.Background(), "", "status"); err == nil {
		t.Error("empty dir: want an error")
	}
}
