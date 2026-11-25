package admincli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeState lays out a workspace root with connector state, a per-run dir and
// things that must never be archived.
func writeState(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("mcp-runs/campaign-codes/REK.json", `{"prefix":"REK","high":3}`)
	mk("mcp-runs/campaign-codes/REK.jsonl", "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n")
	mk("mcp-runs/campaign-codes/.REK.lock", "")
	mk("mcp-runs/email-templates/team/welcome.html", "<p>hi</p>")
	mk("mcp-runs/task-0b6f3c2a/ledger.json", "per-run, swept")
	mk("mcp-runs/stray.txt", "a file directly under mcp-runs is not a state folder")
	mk("conv-1/notes.txt", "a conversation workspace is not connector state")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "mcp-runs/campaign-codes/link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "mcp-runs/evil")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "mcp-runs/campaign-codes/pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// readArchive returns the archive's entry names and file bodies.
func readArchive(t *testing.T, p string) ([]string, map[string]string) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	bodies := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			bodies[hdr.Name] = string(b)
		}
	}
	return names, bodies
}

func TestConnectorStateArchiveKeepsStateOnly(t *testing.T) {
	t.Parallel()
	root := writeState(t)
	out := filepath.Join(t.TempDir(), "fleet-files-20261125T020000Z.tar.gz")

	dirs, err := writeConnectorStateArchive(root, out)
	if err != nil {
		t.Fatalf("writeConnectorStateArchive: %v", err)
	}
	if want := []string{"campaign-codes", "email-templates"}; !slices.Equal(dirs, want) {
		t.Errorf("dirs = %v, want %v", dirs, want)
	}
	names, bodies := readArchive(t, out)
	want := []string{
		"mcp-runs/campaign-codes/",
		"mcp-runs/campaign-codes/.REK.lock",
		"mcp-runs/campaign-codes/REK.json",
		"mcp-runs/campaign-codes/REK.jsonl",
		"mcp-runs/email-templates/",
		"mcp-runs/email-templates/team/",
		"mcp-runs/email-templates/team/welcome.html",
	}
	if !slices.Equal(names, want) {
		t.Errorf("archive holds %v, want %v (no run dir, stray file, conversation, symlink or FIFO)", names, want)
	}
	if got := bodies["mcp-runs/campaign-codes/REK.jsonl"]; got != "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n" {
		t.Errorf("allocation log = %q", got)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("archive mode = %o, want 600", perm)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".fleet-files-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// The runbook restores with plain tar; prove a stock tar reads the archive back
// into a workspace root.
func TestConnectorStateArchiveRestoresWithTar(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar on PATH")
	}
	root := writeState(t)
	out := filepath.Join(t.TempDir(), "fleet-files-x.tar.gz")
	if _, err := writeConnectorStateArchive(root, out); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	cmd := exec.Command("tar", "-xzpf", out, "-C", dest)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar -xzpf: %v: %s", err, b)
	}
	got, err := os.ReadFile(filepath.Join(dest, "mcp-runs/campaign-codes/REK.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"prefix":"REK","high":3}` {
		t.Errorf("restored counter = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "mcp-runs/task-0b6f3c2a")); !os.IsNotExist(err) {
		t.Errorf("a per-run dir was restored: %v", err)
	}
}

func TestConnectorStateArchiveNothingToKeep(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(root string){
		"no mcp-runs": func(string) {},
		"only run dirs": func(root string) {
			if err := os.MkdirAll(filepath.Join(root, "mcp-runs/task-1"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			setup(root)
			out := filepath.Join(t.TempDir(), "fleet-files-x.tar.gz")
			dirs, err := writeConnectorStateArchive(root, out)
			if err != nil || len(dirs) != 0 {
				t.Fatalf("got dirs %v, err %v; want none and no error", dirs, err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("an archive was written with nothing to keep")
			}
		})
	}
}

func TestConnectorStateArchiveRefusesSymlinkedParent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "campaign-codes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "mcp-runs")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "fleet-files-x.tar.gz")
	if _, err := writeConnectorStateArchive(root, out); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("err = %v, want a refusal of the symlinked mcp-runs", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("an archive was written through a symlinked mcp-runs")
	}
}

//nolint:paralleltest // lowers the package-level size cap.
func TestConnectorStateArchiveSizeCap(t *testing.T) {
	old := maxConnectorStateBytes
	maxConnectorStateBytes = 10
	t.Cleanup(func() { maxConnectorStateBytes = old })

	root := writeState(t)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "fleet-files-x.tar.gz")
	if _, err := writeConnectorStateArchive(root, out); err == nil || !strings.Contains(err.Error(), "refusing to archive") {
		t.Fatalf("err = %v, want the size refusal", err)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("files left after the refusal: %v", entries)
	}
}

// backupConnectorState finds the workspace root the way the other operator
// verbs do (FLEET_WORKSPACE_ROOT) and names the archive with the run's stamp.
//
//nolint:paralleltest // sets FLEET_WORKSPACE_ROOT.
func TestBackupConnectorStateUsesWorkspaceRoot(t *testing.T) {
	root := writeState(t)
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	outDir := t.TempDir()
	now := time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)
	if err := backupConnectorState(outDir, now); err != nil {
		t.Fatalf("backupConnectorState: %v", err)
	}
	p := filepath.Join(outDir, "fleet-files-20261126T020000Z.tar.gz")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("archive not written: %v", err)
	}
	if !backupFilePattern.MatchString(filepath.Base(p)) {
		t.Errorf("prune would never remove %s", filepath.Base(p))
	}
}
