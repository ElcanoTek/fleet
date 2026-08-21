// Package diskguard measures free space on the filesystem holding fleet's data
// directory and decides, from that measurement, whether the box should shed
// background load.
//
// fleet already collected disk numbers (internal/hoststats), but only to render
// them in an admin panel: nothing charted them, nothing alerted on them, and
// nothing acted on them. A box that filled its disk discovered the fact when
// Postgres started refusing writes or podman could not create a container —
// which is the worst possible moment, because by then the operator's own
// remedies (open a chat, run a cleanup) are failing too.
//
// The guard closes that loop with one deliberate asymmetry: when free space
// falls below the floor it stops SCHEDULED work and leaves INTERACTIVE chat
// running. A full disk on this box is nearly always produced by unattended runs
// — task workspaces, worktrees, logs, container layers — so stopping them is
// both the effective remedy and the one that preserves the interface an
// operator would use to intervene.
//
// Two properties are load-bearing:
//
//   - It FAILS OPEN. A statfs that errors, or a path that does not exist, is
//     reported as "unavailable" and never as "below the floor". Refusing to run
//     scheduled work because we could not measure the disk would convert a
//     monitoring fault into an outage.
//   - It has HYSTERESIS. Shedding at exactly the floor would flap: the sweep
//     frees a few megabytes, work resumes, the disk fills again. Once shedding,
//     the guard requires a margin above the floor before it resumes.
//
// The package is a leaf: it depends on nothing in fleet, so both the scheduler
// and the HTTP layer can consult it without a dependency cycle.
package diskguard

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SampleTTL is how long a statfs result is reused before the next read
// resamples. The scheduler consults the guard on every claim tick, so caching
// keeps that hot path free of syscalls while staying fresh enough that a disk
// filling over minutes is caught promptly.
const SampleTTL = 15 * time.Second

// RecoveryMarginPercent is the hysteresis band. Once shedding, free space must
// climb this many percentage points ABOVE the floor before scheduled work
// resumes, so a sweep that frees a sliver cannot restart the fill-shed-fill
// cycle.
const RecoveryMarginPercent = 2.0

// Status is one measurement of the guarded filesystem plus the decision derived
// from it. Zero value = unavailable and not shedding, which is the correct
// fail-open reading for a guard that has never sampled.
type Status struct {
	// Available is false when the filesystem could not be measured (missing
	// path, statfs error). Every other numeric field is meaningless then, and
	// Shedding is always false.
	Available bool `json:"available"`
	// Path is the filesystem location that was measured.
	Path string `json:"path"`
	// TotalBytes and FreeBytes are the capacity and the space available to an
	// unprivileged writer (statfs Bavail, not Bfree — the root reserve is not
	// space fleet can use).
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	// FreePercent is FreeBytes as a percentage of TotalBytes.
	FreePercent float64 `json:"free_percent"`
	// MinFreePercent is the configured floor (0 = the guard is disabled).
	MinFreePercent int `json:"min_free_percent"`
	// Shedding reports the guard's decision: scheduled work should not be
	// claimed while it is true. Interactive chat is never gated on it.
	Shedding bool `json:"shedding"`
	// Err carries the sample failure, if any, for operator display. It never
	// causes shedding.
	Err string `json:"error,omitempty"`
	// SampledAt is when the underlying statfs ran.
	SampledAt time.Time `json:"sampled_at"`
}

// Reason renders a short operator-facing explanation of a shedding decision,
// suitable for a log line or a health payload. Empty when not shedding.
func (s Status) Reason() string {
	if !s.Shedding {
		return ""
	}
	return "disk below the free-space floor"
}

// Guard samples a path's free space and applies the floor with hysteresis.
// Safe for concurrent use. The zero value is not usable — construct with New.
type Guard struct {
	path           string
	minFreePercent int

	// statfs is the measurement seam. Production wires the syscall; tests
	// substitute a deterministic function so the floor and the hysteresis can
	// be exercised without a real filesystem.
	statfs func(path string) (total, free uint64, err error)
	now    func() time.Time
	ttl    time.Duration

	mu       sync.Mutex
	last     Status
	shedding bool
}

// New builds a Guard for path with a floor of minFreePercent. A non-positive
// floor disables the decision entirely: the guard still samples (so the metrics
// and the admin panel keep working) but never sheds.
func New(path string, minFreePercent int) *Guard {
	if minFreePercent < 0 {
		minFreePercent = 0
	}
	return &Guard{
		path:           path,
		minFreePercent: minFreePercent,
		statfs:         statfsBytes,
		now:            time.Now,
		ttl:            SampleTTL,
	}
}

// NewWithSampler is New with the measurement function supplied by the caller.
//
// It exists because the guard's interesting behaviour — the floor, the
// hysteresis, the fail-open — is only observable at specific free-space levels,
// and a test cannot fill a real disk to reach them. Callers outside this
// package (the /readyz probe's tests) need the same control.
//
// sampler must return (total, free) in bytes, or an error the guard treats as
// "unmeasurable" and therefore fails open on. A nil sampler falls back to Usage.
func NewWithSampler(path string, minFreePercent int, sampler func(path string) (total, free uint64, err error)) *Guard {
	g := New(path, minFreePercent)
	if sampler != nil {
		g.statfs = sampler
	}
	return g
}

// Enabled reports whether the guard will ever shed. Sampling and reporting
// happen regardless; this is only about the decision.
func (g *Guard) Enabled() bool { return g != nil && g.minFreePercent > 0 }

// Status returns the current measurement, resampling if the cached one has aged
// past the TTL. A nil Guard returns the zero Status, so callers that never wired
// one need no nil checks.
func (g *Guard) Status() Status {
	if g == nil {
		return Status{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last.SampledAt.IsZero() || g.now().Sub(g.last.SampledAt) >= g.ttl {
		g.sampleLocked()
	}
	return g.last
}

// Shedding reports whether scheduled work should be held back right now. A nil
// or disabled guard never sheds.
func (g *Guard) Shedding() bool { return g.Status().Shedding }

// Sample forces a fresh measurement, bypassing the TTL. Used at boot (so the
// first log line and the first scrape reflect reality) and by tests.
func (g *Guard) Sample() Status {
	if g == nil {
		return Status{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sampleLocked()
	return g.last
}

// sampleLocked performs the measurement and applies the floor. Caller holds mu.
func (g *Guard) sampleLocked() {
	now := g.now()
	// Measure the deepest EXISTING ancestor of the configured path, not the
	// path itself: fleet creates its data directory lazily (the first upload
	// makes temp_uploads, and so on), so on a fresh box the guard would
	// otherwise report "unmeasurable" and log a warning at every boot until
	// something happened to write there. An ancestor sits on the same
	// filesystem in the normal case, so the answer is the same one.
	measured := existingAncestor(g.path)
	total, free, err := g.statfs(measured)
	st := Status{
		Path:           measured,
		MinFreePercent: g.minFreePercent,
		SampledAt:      now,
	}
	switch {
	case err != nil:
		// FAIL OPEN: an unmeasurable filesystem must not stop the scheduler.
		// Clear any latched shedding state too — we can no longer justify it.
		st.Err = err.Error()
		g.shedding = false
	case total == 0:
		st.Err = "filesystem reports zero capacity"
		g.shedding = false
	default:
		st.Available = true
		st.TotalBytes = total
		st.FreeBytes = free
		st.FreePercent = float64(free) / float64(total) * 100
		g.shedding = g.decideLocked(st.FreePercent)
	}
	st.Shedding = g.shedding
	g.last = st
}

// existingAncestor returns path if it exists, else its nearest existing parent,
// else path unchanged (so the caller still gets a real statfs error to report).
//
// The one case it gets "wrong" is a data dir on its own mount that has not been
// mounted yet: the parent then describes a different filesystem. Reporting the
// parent's headroom is still far better than reporting nothing, and Status.Path
// names what was actually measured so an operator can see the substitution.
func existingAncestor(path string) string {
	if path == "" {
		return path
	}
	for cur := filepath.Clean(path); ; {
		if _, err := os.Stat(cur); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the root without finding anything (only possible on a
			// broken mount): let the caller's statfs report the real error.
			return path
		}
		cur = parent
	}
}

// decideLocked applies the floor with hysteresis: crossing BELOW the floor
// starts shedding, and only climbing above floor+margin stops it. Caller holds
// mu.
func (g *Guard) decideLocked(freePercent float64) bool {
	if g.minFreePercent <= 0 {
		return false
	}
	floor := float64(g.minFreePercent)
	if g.shedding {
		// Already shedding: require the recovery margin before resuming.
		return freePercent < floor+RecoveryMarginPercent
	}
	return freePercent < floor
}
