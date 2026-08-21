package diskguard

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// fixedGuard builds a Guard whose measurement and clock are deterministic. The
// returned setters let a test move free space and time independently.
func fixedGuard(t *testing.T, minFreePercent int) (g *Guard, setFree func(freePercent float64), setErr func(error), advance func(time.Duration)) {
	t.Helper()
	const total = uint64(100 << 30) // 100 GiB, so percentages are exact enough
	var (
		free    = total
		sampErr error
		now     = time.Unix(1_700_000_000, 0)
	)
	g = New("/data", minFreePercent)
	g.statfs = func(string) (uint64, uint64, error) {
		if sampErr != nil {
			return 0, 0, sampErr
		}
		return total, free, nil
	}
	g.now = func() time.Time { return now }
	return g,
		func(pct float64) { free = uint64(float64(total) * pct / 100) },
		func(err error) { sampErr = err },
		func(d time.Duration) { now = now.Add(d) }
}

func TestGuardShedsBelowFloor(t *testing.T) {
	g, setFree, _, _ := fixedGuard(t, 5)

	setFree(20)
	if st := g.Sample(); st.Shedding {
		t.Fatalf("20%% free with a 5%% floor must not shed: %+v", st)
	}

	setFree(3)
	st := g.Sample()
	if !st.Shedding {
		t.Fatalf("3%% free with a 5%% floor must shed: %+v", st)
	}
	if st.Reason() == "" {
		t.Error("a shedding status must carry an operator-facing reason")
	}
}

// Without hysteresis a sweep that frees a sliver restarts the fill-shed-fill
// cycle, so recovery must require a margin above the floor.
func TestGuardHysteresis(t *testing.T) {
	g, setFree, _, _ := fixedGuard(t, 5)

	setFree(3)
	if !g.Sample().Shedding {
		t.Fatal("expected to be shedding below the floor")
	}

	// Just above the floor but inside the recovery margin: still shedding.
	setFree(5.5)
	if !g.Sample().Shedding {
		t.Fatalf("recovery inside the %.0f-point margin must not clear shedding", RecoveryMarginPercent)
	}

	// Clear of the margin: resume.
	setFree(8)
	if g.Sample().Shedding {
		t.Fatal("free space past floor+margin must clear shedding")
	}
}

// The single most important property: a guard that cannot measure the disk must
// never stop the scheduler. A monitoring fault is not an outage.
func TestGuardFailsOpenOnStatfsError(t *testing.T) {
	g, setFree, setErr, _ := fixedGuard(t, 5)

	setFree(1)
	if !g.Sample().Shedding {
		t.Fatal("expected shedding at 1% free")
	}

	setErr(errors.New("statfs: no such file or directory"))
	st := g.Sample()
	if st.Shedding {
		t.Fatalf("an unmeasurable filesystem must fail open, got %+v", st)
	}
	if st.Available {
		t.Error("a failed sample must report Available=false")
	}
	if st.Err == "" {
		t.Error("a failed sample must carry the error for operator display")
	}
}

func TestGuardZeroCapacityFailsOpen(t *testing.T) {
	g := New("/data", 5)
	g.statfs = func(string) (uint64, uint64, error) { return 0, 0, nil }
	st := g.Sample()
	if st.Shedding || st.Available {
		t.Fatalf("a zero-capacity filesystem must fail open and report unavailable: %+v", st)
	}
}

// A zero floor disables the decision but must keep sampling, so the metrics and
// the admin panel still work on a box that has opted out of backpressure.
func TestGuardDisabledStillSamples(t *testing.T) {
	g, setFree, _, _ := fixedGuard(t, 0)
	setFree(0.1)

	st := g.Sample()
	if st.Shedding {
		t.Fatal("a zero floor must never shed")
	}
	if !st.Available || st.TotalBytes == 0 {
		t.Fatalf("a disabled guard must still report a measurement: %+v", st)
	}
	if g.Enabled() {
		t.Error("Enabled() must be false for a zero floor")
	}
}

func TestGuardCachesWithinTTL(t *testing.T) {
	g, setFree, _, advance := fixedGuard(t, 5)
	calls := 0
	inner := g.statfs
	g.statfs = func(p string) (uint64, uint64, error) { calls++; return inner(p) }

	setFree(50)
	g.Status()
	g.Status()
	g.Status()
	if calls != 1 {
		t.Fatalf("statfs called %d times inside the TTL; want 1", calls)
	}

	advance(SampleTTL + time.Second)
	g.Status()
	if calls != 2 {
		t.Fatalf("statfs called %d times after the TTL elapsed; want 2", calls)
	}
}

// Callers that never wired a guard must not need nil checks.
func TestNilGuardIsInert(t *testing.T) {
	var g *Guard
	if g.Enabled() || g.Shedding() {
		t.Fatal("a nil guard must be inert")
	}
	if st := g.Status(); st.Available || st.Shedding {
		t.Fatalf("a nil guard must return the zero status, got %+v", st)
	}
}

func TestNegativeFloorIsClamped(t *testing.T) {
	g := New("/data", -10)
	if g.Enabled() {
		t.Fatal("a negative floor must clamp to disabled, not invert the comparison")
	}
}

// fleet creates its data directory lazily, so on a fresh box the configured
// path does not exist yet. Measuring the nearest existing ancestor keeps the
// guard useful (and quiet) instead of reporting "unmeasurable" at every boot.
func TestGuardMeasuresNearestExistingAncestor(t *testing.T) {
	base := t.TempDir()
	notYetCreated := filepath.Join(base, "data", "attachments", "uploads")

	var measured string
	g := NewWithSampler(notYetCreated, 5, func(path string) (uint64, uint64, error) {
		measured = path
		return 100 << 30, 50 << 30, nil
	})

	st := g.Sample()
	if measured != base {
		t.Fatalf("measured %q, want the nearest existing ancestor %q", measured, base)
	}
	if !st.Available {
		t.Errorf("a lazily-created data dir must not read as unmeasurable: %+v", st)
	}
	// Status.Path names what was actually measured, so the substitution is
	// visible to an operator rather than silent.
	if st.Path != base {
		t.Errorf("Status.Path = %q, want the measured path %q", st.Path, base)
	}
}

// Once the directory exists it is measured directly, with no substitution.
func TestGuardMeasuresThePathWhenItExists(t *testing.T) {
	dir := t.TempDir()
	var measured string
	g := NewWithSampler(dir, 5, func(path string) (uint64, uint64, error) {
		measured = path
		return 100 << 30, 50 << 30, nil
	})
	g.Sample()
	if measured != dir {
		t.Fatalf("measured %q, want the configured path %q", measured, dir)
	}
}

func TestExistingAncestorEdgeCases(t *testing.T) {
	if got := existingAncestor(""); got != "" {
		t.Errorf("empty path should pass through, got %q", got)
	}
	if got := existingAncestor("/"); got != "/" {
		t.Errorf("root should resolve to itself, got %q", got)
	}
}
