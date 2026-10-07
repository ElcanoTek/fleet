package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// writeWalkFixture writes n files into convID's workspace, file i modified i
// seconds after a fixed base, plus an upload that must never be listed.
func writeWalkFixture(t *testing.T, convID string, n int) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	ws := filepath.Join(root, convID)
	base := time.Unix(1_700_000_000, 0)
	for i := range n {
		p := filepath.Join(ws, "d", fmt.Sprintf("f%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	up := filepath.Join(ws, uploadsDir, "u", "in.pdf")
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(up, []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// More files than the cap: only the newest `limit` are kept, newest first,
// and the listing says it was truncated.
func TestWalkWorkspaceFilesKeepsNewestBoundedSet(t *testing.T) {
	const n = maxProjectFiles + 57
	writeWalkFixture(t, "c1", n)
	got, truncated := walkWorkspaceFiles(context.Background(), "c1", maxProjectFiles)
	if !truncated {
		t.Error("more files than the cap must report truncated")
	}
	if len(got) != maxProjectFiles {
		t.Fatalf("len = %d, want %d", len(got), maxProjectFiles)
	}
	for i, f := range got {
		want := fmt.Sprintf("d/f%04d.txt", n-1-i)
		if f.Path != want {
			t.Fatalf("got[%d] = %s, want %s (newest first)", i, f.Path, want)
		}
	}

	// Exactly at the cap: everything, not truncated; uploads never listed.
	writeWalkFixture(t, "c2", 5)
	got, truncated = walkWorkspaceFiles(context.Background(), "c2", 5)
	if truncated || len(got) != 5 || got[0].Path != "d/f0004.txt" || got[4].Path != "d/f0000.txt" {
		t.Errorf("at cap: %v truncated=%v", got, truncated)
	}

	// No workspace at all: empty, not truncated.
	if got, truncated := walkWorkspaceFiles(context.Background(), "nope", 5); len(got) != 0 || truncated {
		t.Errorf("missing workspace: %v %v", got, truncated)
	}
}

// Ties on modtime break by path, as the old full sort did.
func TestWalkWorkspaceFilesTieOrder(t *testing.T) {
	writeWalkFixture(t, "c", 0)
	ws := filepath.Join(os.Getenv("FLEET_WORKSPACE_ROOT"), "c")
	mt := time.Unix(1_700_000_000, 0)
	for _, name := range []string{"b.txt", "a.txt", "c.txt", "d.txt"} {
		p := filepath.Join(ws, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	got, truncated := walkWorkspaceFiles(context.Background(), "c", 3)
	if !truncated || len(got) != 3 || got[0].Path != "a.txt" || got[1].Path != "b.txt" || got[2].Path != "c.txt" {
		t.Errorf("tie order = %v truncated=%v", got, truncated)
	}
}

// The walk itself is bounded: past the visit budget it stops and reports
// truncated rather than statting a runaway tree whole.
func TestWalkWorkspaceFilesVisitBudget(t *testing.T) {
	writeWalkFixture(t, "c", 50)
	old := maxWorkspaceWalkEntries
	maxWorkspaceWalkEntries = 20
	t.Cleanup(func() { maxWorkspaceWalkEntries = old })
	got, truncated := walkWorkspaceFiles(context.Background(), "c", maxProjectFiles)
	if !truncated {
		t.Error("hitting the visit budget must report truncated")
	}
	if len(got) == 0 || len(got) >= 20 {
		t.Errorf("visited past the budget: %d files", len(got))
	}
}

// A walk under an expired context stops at once and says it was cut short.
func TestWalkWorkspaceFilesStopsAtDeadline(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "c", "a.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, truncated := walkWorkspaceFiles(ctx, "c", maxProjectFiles); len(got) != 0 || !truncated {
		t.Errorf("walk under a done context = %v truncated=%v, want nothing and truncated", got, truncated)
	}
}

// A half whose deadline passes DURING an examination stops there and reports
// a cut — never a request error — and the team half still gets time of its
// own however late the first half finished.
func TestSourcesHalfDeadlineCutsAndTeamHalfKeepsItsTime(t *testing.T) {
	oldN, oldT := maxSourcesDiscoveries, sourcesDiscoveryBudget
	t.Cleanup(func() { maxSourcesDiscoveries, sourcesDiscoveryBudget = oldN, oldT })
	maxSourcesDiscoveries, sourcesDiscoveryBudget = 10, 100*time.Millisecond

	first, rest := newSourcesBudgets()
	list := []store.Conversation{{ID: "slow"}, {ID: "next"}}
	var examined []string
	done := false
	cut, err := listSourcesHalf(context.Background(), list, "", &done, first,
		func(ctx context.Context, c store.Conversation) (bool, error) {
			examined = append(examined, c.ID)
			<-ctx.Done() // a discovery that outlasts the half's deadline
			return false, ctx.Err()
		})
	if err != nil || !cut || strings.Join(examined, ",") != "slow" {
		t.Fatalf("first half: cut=%v err=%v examined=%v, want a cut after the slow chat and no error", cut, err, examined)
	}
	time.Sleep(sourcesDiscoveryBudget) // the first half overran the whole request window
	if !rest().spend() {
		t.Error("the team half got no time after a first half that overran")
	}
}

// A stalled filesystem cannot hold Sources past its deadline: the output
// stats and the workspace walk are abandoned when ctx is done, even while a
// filesystem call is still stuck.
func TestOutputStatsAndWalkAreAbandonedAtDeadline(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "c", "a.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The abandoned goroutines are still parked in the hooks when the test
	// ends: release them and wait for every hook call to return before the
	// hooks are cleared: one stat (a.csv) and one walk entry (the root —
	// by the time it is released the deadline has passed, so the walk stops
	// there).
	release := make(chan struct{})
	returned := make(chan struct{}, 2)
	outputStatHook = func(string) { <-release; returned <- struct{}{} }
	workspaceWalkHook = func() { <-release; returned <- struct{}{} }
	t.Cleanup(func() {
		close(release)
		for range 2 {
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Error("a parked filesystem call never returned")
			}
		}
		outputStatHook, workspaceWalkHook = nil, nil
	})

	history := []agent.HistoryEntry{{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"[a](a.csv)"}`)}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := conversationOutputsCtx(ctx, "c", history, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stalled stats: err = %v, want DeadlineExceeded", err)
	}
	if files, truncated := walkWorkspaceFilesAbandonable(ctx, "c", maxProjectFiles); len(files) != 0 || !truncated {
		t.Errorf("stalled walk = %v truncated=%v, want nothing and truncated", files, truncated)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("returned after %v; a stalled filesystem must not hold Sources", el)
	}
}

// Abandoned filesystem workers are capped: with every slot held (a stalled
// mount), output resolution and the walk fail fast instead of starting more
// workers, and Sources reports that as a cut, not an error.
func TestFilesystemWorkersAreCapped(t *testing.T) {
	for range maxFSWorkers {
		fsWorkerSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range maxFSWorkers {
			<-fsWorkerSlots
		}
	})
	if _, _, err := conversationOutputsCtx(context.Background(), "c", nil, nil); !errors.Is(err, errFilesystemBusy) {
		t.Errorf("outputs with every slot held: err = %v, want errFilesystemBusy", err)
	}
	if files, truncated := walkWorkspaceFilesAbandonable(context.Background(), "c", maxProjectFiles); len(files) != 0 || !truncated {
		t.Errorf("walk with every slot held = %v truncated=%v, want nothing and truncated", files, truncated)
	}
	first, _ := newSourcesBudgets()
	done := false
	cut, err := listSourcesHalf(context.Background(), []store.Conversation{{ID: "a"}}, "", &done, first,
		func(context.Context, store.Conversation) (bool, error) { return false, errFilesystemBusy })
	if err != nil || !cut {
		t.Errorf("Sources half with a busy filesystem: cut=%v err=%v, want a cut and no error", cut, err)
	}
}

// The focused chat, examined past the count cap, still runs under a deadline
// of its own: a stalled examination ends as a cut, never a hung request.
func TestFocusedSourcesDiscoveryIsBounded(t *testing.T) {
	oldN, oldF := maxSourcesDiscoveries, sourcesFocusBudget
	t.Cleanup(func() { maxSourcesDiscoveries, sourcesFocusBudget = oldN, oldF })
	maxSourcesDiscoveries, sourcesFocusBudget = 0, 100*time.Millisecond // no budget: only the focus runs

	first, _ := newSourcesBudgets()
	done := false
	start := time.Now()
	cut, err := listSourcesHalf(context.Background(), []store.Conversation{{ID: "focus"}}, "focus", &done, first,
		func(ctx context.Context, _ store.Conversation) (bool, error) {
			<-ctx.Done() // a stalled filesystem
			return false, ctx.Err()
		})
	if err != nil || !cut || !done {
		t.Errorf("stalled focus: cut=%v err=%v done=%v, want a cut, no error, focus handled", cut, err, done)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("focused discovery held the request for %v", el)
	}
}
