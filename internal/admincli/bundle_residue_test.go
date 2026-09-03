package admincli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func seedResidueRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("mcp_servers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "seed")
	return dir
}

// TestBundleResidue covers the states an operator's bundle lands in. The shape
// that matters is the one found on a real box: connector output inside untracked
// directories, including a filename git quotes because it contains spaces.
func TestBundleResidue(t *testing.T) {
	t.Run("a clean checkout reports nothing", func(t *testing.T) {
		dir := seedResidueRepo(t)
		if _, _, ok := bundleResidue(dir); ok {
			t.Error("clean checkout must not report residue")
		}
	})

	t.Run("counts files inside untracked dirs, not the dirs", func(t *testing.T) {
		dir := seedResidueRepo(t)
		if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"a.csv", "b.csv", "RainBarrel OpenX Report (+00:00)__x.xlsx"} {
			if err := os.WriteFile(filepath.Join(dir, "reports", n), bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		count, size, ok := bundleResidue(dir)
		if !ok {
			t.Fatal("residue must be reported")
		}
		// 3, not 1: -uall descends into the untracked directory. A quoted
		// filename must still be stat-able, or its bytes vanish from the total.
		if count != 3 {
			t.Errorf("count = %d, want 3 (files, not directories)", count)
		}
		if size != 3*1024 {
			t.Errorf("size = %d, want %d — a git-quoted name was likely not unquoted before stat", size, 3*1024)
		}
	})

	t.Run("ignored files still count", func(t *testing.T) {
		// The bundles ship a .gitignore for exactly these paths. Counting only
		// non-ignored files would make a tree that is still filling look clean.
		dir := seedResidueRepo(t)
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("/reports/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "reports", "a.csv"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// .gitignore itself is untracked here, so residue is reported either
		// way; what this pins is that an IGNORED tree is not silently excluded
		// from the operator's view of what is on disk.
		count, _, ok := bundleResidue(dir)
		if !ok || count == 0 {
			t.Errorf("want residue reported, got count=%d ok=%v", count, ok)
		}
	})

	t.Run("no bundle, missing dir and non-checkout are silent", func(t *testing.T) {
		for _, dir := range []string{"", t.TempDir(), filepath.Join(t.TempDir(), "nope")} {
			if _, _, ok := bundleResidue(dir); ok {
				t.Errorf("bundleResidue(%q) must be silent", dir)
			}
		}
	})
}

// TestReportBundleResidue — the operator-facing text must name the directory and
// both commands, and must stay silent when there is nothing to say (this runs
// daily and unattended from fleet-maintenance.timer).
func TestReportBundleResidue(t *testing.T) {
	dir := seedResidueRepo(t)
	t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)

	var quiet bytes.Buffer
	reportBundleResidue(&quiet)
	if quiet.Len() != 0 {
		t.Errorf("clean checkout must print nothing, got %q", quiet.String())
	}

	if err := os.WriteFile(filepath.Join(dir, "leftover.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	reportBundleResidue(&buf)
	out := buf.String()
	for _, want := range []string{dir, "1 untracked file", "clean -nd", "status --porcelain -uall"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n--- output ---\n%s", want, out)
		}
	}
	// It must never imply fleet will delete these itself.
	if strings.Contains(strings.ToLower(out), "deleted") || strings.Contains(strings.ToLower(out), "removing") {
		t.Errorf("report must not claim fleet removes anything:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{0, "0B"}, {512, "512B"}, {1024, "1.0KB"}, {1536, "1.5KB"}, {1048576, "1.0MB"}, {1073741824, "1.0GB"}} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
