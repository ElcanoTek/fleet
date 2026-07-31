package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePrunePodman writes a shell script that stands in for podman: `ps` prints
// psOutput, `rm` records its arguments to rm.args. Returns the script path and
// the rm.args path.
func fakePrunePodman(t *testing.T, psOutput string) (script, rmArgs string) {
	t.Helper()
	dir := t.TempDir()
	psFile := filepath.Join(dir, "ps.out")
	if err := os.WriteFile(psFile, []byte(psOutput), 0o600); err != nil {
		t.Fatalf("write ps.out: %v", err)
	}
	rmArgs = filepath.Join(dir, "rm.args")
	script = filepath.Join(dir, "podman")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  ps) cat '" + psFile + "' ;;\n" +
		"  rm) shift; echo \"$@\" > '" + rmArgs + "' ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return script, rmArgs
}

// TestPruneOrphanedContainers_ScopedByInstanceLabel verifies the ownership
// scoping: the boot-time sweep must reclaim genuinely orphaned containers
// (exited, or running with a dead owner pid in the fleet.instance label) while
// NEVER rm -f'ing a running container owned by a live fleet process — or one
// whose ownership cannot be established at all. Before the fix the sweep
// removed everything matching the name prefix, killing a sibling instance's
// live sandboxes on every boot of a shared podman user.
func TestPruneOrphanedContainers_ScopedByInstanceLabel(t *testing.T) {
	const livePID = "31337"
	psOutput := strings.Join([]string{
		"aaa|running|4242@100",             // owner pid dead → crashed prior run → remove
		"bbb|running|" + thisInstanceLabel, // this instance's own container → skip
		"ccc|exited|4242@100",              // not running → always remove
		"ddd|running|",                     // unlabeled (pre-label build) but running → skip
		"eee|running|" + livePID + "@50",   // live sibling instance → skip
		"fff|created|",                     // not running → always remove
		"ggg|running|not-a-label",          // malformed label, can't attribute → skip
	}, "\n") + "\n"
	script, rmArgs := fakePrunePodman(t, psOutput)

	origPidAlive := pidAlive
	t.Cleanup(func() { pidAlive = origPidAlive })
	pidAlive = func(pid int) bool {
		return pid == os.Getpid() || pid == 31337
	}

	removed, err := PruneOrphanedContainers(context.Background(), script)
	if err != nil {
		t.Fatalf("PruneOrphanedContainers: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}
	got, err := os.ReadFile(rmArgs)
	if err != nil {
		t.Fatalf("read recorded rm args: %v", err)
	}
	if want := "-f aaa ccc fff"; strings.TrimSpace(string(got)) != want {
		t.Errorf("rm args = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

// TestPruneOrphanedContainers_NothingToPrune covers the boring boot: no
// leftover containers means zero removals, no error, and no `podman rm` call.
func TestPruneOrphanedContainers_NothingToPrune(t *testing.T) {
	script, rmArgs := fakePrunePodman(t, "\n")

	removed, err := PruneOrphanedContainers(context.Background(), script)
	if err != nil {
		t.Fatalf("PruneOrphanedContainers: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if _, err := os.Stat(rmArgs); !os.IsNotExist(err) {
		t.Errorf("podman rm must not run when there is nothing to prune (stat err = %v)", err)
	}
}

func TestInstanceLabelPID(t *testing.T) {
	cases := []struct {
		label string
		pid   int
		ok    bool
	}{
		{"1234@567", 1234, true},
		{"", 0, false},
		{"no-at-sign", 0, false},
		{"abc@567", 0, false},
		{"-5@567", 0, false},
		{"0@567", 0, false},
	}
	for _, tc := range cases {
		pid, ok := instanceLabelPID(tc.label)
		if pid != tc.pid || ok != tc.ok {
			t.Errorf("instanceLabelPID(%q) = (%d, %v), want (%d, %v)", tc.label, pid, ok, tc.pid, tc.ok)
		}
	}
}
