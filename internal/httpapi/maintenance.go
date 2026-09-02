package httpapi

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The chat plane's reclamation pass, and the two ways it is driven.
//
// Every sweep below already existed; what did not exist was a driver that runs
// them independently of chat traffic. They were called from exactly one place —
// the tail of a completed turn — which made reclamation a function of how busy
// the box was, in both directions and both wrong:
//
//   - An idle box never reclaimed at all. A scheduler-only deployment (no
//     interactive users), or any box whose chat goes quiet for a week, kept
//     every expired conversation, every terminal input-queue row, every
//     aged-out turn ledger and every orphaned workspace directory forever. The
//     hourly disk sweep in cmd/fleet covered attachments and temp uploads only,
//     so the DB-side growth had no bound at all.
//   - A busy box reclaimed far too often. Ten concurrent turns each finished by
//     running three global DB sweeps plus a full recursive walk of the
//     attachment tree and a readdir of the workspace root — the same work, N
//     times an hour, contending on the same rows, inline on the turn goroutine
//     so it landed in the user's turn-completion latency.
//
// So the pass now has ONE implementation (RunMaintenance) and TWO drivers:
//
//  1. cmd/fleet's maintenance ticker calls RunMaintenance on a fixed interval.
//     That is the guarantee: reclamation happens whether or not anyone chats.
//  2. The post-turn path calls runPostTurnMaintenance, which is the same pass
//     behind a rate gate. It is now an optimization — it reclaims promptly
//     after the turn that produced the garbage — rather than the only driver.
//
// The gate is a compare-and-swap on a single timestamp, so concurrent turns
// cannot all pass it: exactly one wins the interval and the rest return
// immediately.

// maintenanceMinInterval bounds how often the OPPORTUNISTIC post-turn pass
// actually does work. It does not bound the ticker in cmd/fleet — that one is
// the guarantee and always runs.
//
// Five minutes is deliberately much shorter than the ticker: a busy box still
// reclaims promptly after the turns that produce the garbage, it just stops
// doing it once per turn. FLEET_MAINTENANCE_MIN_INTERVAL overrides it; a
// non-positive value disables the gate entirely (every turn sweeps, the
// pre-existing behaviour) for operators who want it back.
//
// Held in an atomic so a reader on a post-turn goroutine and a writer that
// swaps the width (the tests do; production sets it once at init) never race.
var maintenanceMinInterval = newAtomicDuration(envDuration("FLEET_MAINTENANCE_MIN_INTERVAL", 5*time.Minute))

// atomicDuration is a time.Duration readable and writable without a lock.
type atomicDuration struct{ ns atomic.Int64 }

func newAtomicDuration(d time.Duration) *atomicDuration {
	a := &atomicDuration{}
	a.Store(d)
	return a
}

func (a *atomicDuration) Load() time.Duration   { return time.Duration(a.ns.Load()) }
func (a *atomicDuration) Store(d time.Duration) { a.ns.Store(int64(d)) }

// DefaultMaintenanceInterval is how often cmd/fleet's ticker runs the full
// pass. Hourly matches the disk sweep it replaces and is frequent enough that
// no single interval accumulates a meaningful amount of garbage, while being
// far cheaper than the per-turn cadence it supersedes.
const DefaultMaintenanceInterval = time.Hour

// RunMaintenance runs the chat plane's full reclamation pass: the database
// retention sweeps (expired conversations, terminal input-queue rows, aged-out
// turn ledgers), the attachment-file sweep, and the orphan-workspace sweep.
//
// Every step is best-effort and independent: one failing logs and the rest
// still run, because a sweep that cannot complete must never block the sweeps
// that can. Each step's own store method treats a non-positive TTL as
// "disabled", so an operator who has turned a retention knob off gets a no-op
// rather than a surprise deletion.
//
// Safe to call concurrently with itself and with a post-turn pass — the
// underlying sweeps are all idempotent set-based deletions — though the rate
// gate means that is rare in practice. Exported so cmd/fleet can drive it.
func (s *Server) RunMaintenance(ctx context.Context) {
	s.sweepRetention(ctx)

	// Attachment files whose conversation TTL has elapsed. Walks the tree, so
	// this is the expensive step and the main reason the per-turn cadence was
	// worth removing.
	if removed, err := store.SweepAttachments(s.cfg.EmailAttachmentDir,
		time.Duration(s.cfg.ConversationTTL)*24*time.Hour); err != nil {
		log.Printf("maintenance: attachment sweep: %v", err)
	} else if removed > 0 {
		log.Printf("maintenance: attachment sweep removed %d file(s)", removed)
	}

	// Per-conversation workspace dirs whose conversation row is gone (TTL,
	// cap-evict, or a user-account delete that cascaded). Anything the agent
	// downloaded into <root>/<convID>/ goes with them. Ordered after
	// sweepRetention so rows that expired on THIS pass have their directories
	// reclaimed on the same pass rather than the next one.
	if removed, err := s.store.SweepOrphanWorkspaces(ctx,
		tools.WorkspaceDirForConversation("")); err != nil {
		log.Printf("maintenance: workspace sweep: %v", err)
	} else if removed > 0 {
		log.Printf("maintenance: workspace sweep removed %d orphan dir(s)", removed)
	}

	// Reconcile the shared file library's staged tree against its manifest
	// (docs/SHARED-FILES.md): re-stage anything missing or wrong-sized, remove
	// strays. This is what makes the mutating endpoints' staging "best-effort,
	// self-healing" an honest claim rather than a hope.
	if err := s.SyncSharedFiles(ctx); err != nil {
		log.Printf("maintenance: shared files sync: %v", err)
	}
}

// runPostTurnMaintenance runs the pass at most once per maintenanceMinInterval,
// no matter how many turns finish in that window.
//
// The claim is a compare-and-swap rather than a load-then-store: N turns
// finishing at the same instant all read the same stale timestamp, so a
// non-atomic check would let every one of them through and reintroduce exactly
// the stampede the gate exists to prevent. Only the CAS winner sweeps.
func (s *Server) runPostTurnMaintenance(ctx context.Context) {
	if !s.claimMaintenanceSlot(time.Now()) {
		return
	}
	s.RunMaintenance(ctx)
}

// claimMaintenanceSlot reports whether the caller may run the pass now, and
// claims the interval if so. A non-positive maintenanceMinInterval disables the
// gate (always true), restoring the pre-gate every-turn behaviour.
func (s *Server) claimMaintenanceSlot(now time.Time) bool {
	minInterval := maintenanceMinInterval.Load()
	if minInterval <= 0 {
		return true
	}
	nowUnix := now.UnixNano()
	for {
		last := s.lastMaintenance.Load()
		// A zero timestamp means "never swept" — the first turn after boot
		// always sweeps, so a short-lived process still reclaims once.
		if last != 0 && now.Sub(time.Unix(0, last)) < minInterval {
			return false
		}
		if s.lastMaintenance.CompareAndSwap(last, nowUnix) {
			return true
		}
		// Lost the race to another turn; loop to re-read. The re-read sees the
		// winner's timestamp and returns false.
	}
}

// NoteMaintenanceRun records that an EXTERNAL driver (cmd/fleet's ticker) just
// completed a pass, so the post-turn gate counts it too. Without this the
// ticker and the post-turn path would each keep their own idea of "recently
// swept" and the box would do roughly twice the work it needs to.
func (s *Server) NoteMaintenanceRun(at time.Time) {
	s.lastMaintenance.Store(at.UnixNano())
}
