package sandbox

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// Pool keeps a small number of pre-spawned sandboxes warm so the first
// run_python (and first bash) call of a turn doesn't pay the
// container-spin + python-boot + pandas-import cost.
//
// Mirrors the lifecycle the legacy KernelPool had:
//
//   - At Manager boot, NewPool(cfg) eagerly spawns cfg.Size sandboxes.
//   - At turn start, Take() returns a Sandbox + cleanup. Take also
//     fires a goroutine to refill so the next turn is warm.
//   - cleanup() closes the sandbox; warm members are never recycled
//     across turns — fresh containers for fresh turns.
//
// The real invariant is NARROWER than "fresh per turn": a sandbox is NEVER
// SHARED ACROSS CONVERSATIONS. That is what avoids the OpenAI-2024-style leak
// where a "warm" sandbox carried files from the previous conversation into the
// next. Per-turn freshness was the original way to guarantee it. Persistent
// REPL mode (#213, see persistent.go) is a SECOND way that also upholds it: it
// keeps one sandbox alive across the turns of a SINGLE conversation (keyed by
// conversation ID), so the python kernel's state survives between turns, while
// still never crossing a conversation boundary.
//
// Failure model: when container construction fails (misconfigured
// rootless-podman, dead daemon, image gone), Take() returns the error
// to the caller. The agent surfaces it to the model as a tool error
// ("sandbox unavailable"), the user sees that on the chip, and the
// operator sees the underlying podman error in chat-server logs.
// There is no "degrade to host" path — host execution would let
// agent-emitted code escape the container boundary, which is the
// whole point of the sandbox.
//
// Pool is safe for concurrent Take. Pass cfg.Size = 0 to disable warming
// (Take cold-starts every time — useful for tests with podman
// available but no need to amortize).
type Pool struct {
	cfg PoolConfig

	mu     sync.Mutex
	slots  chan parkedSandbox
	closed bool
	// done is closed by Close to stop the TTL keeper goroutine. nil when no
	// keeper runs (Size<=0 or WarmTTL<=0).
	done chan struct{}

	// nowFn is the clock the warm-TTL logic reads. Defaults to time.Now;
	// overridden in tests to exercise reaping deterministically.
	nowFn func() time.Time

	// storageProbeOnce caches the one-time --storage-opt support probe (#216) so
	// the disk-quota mechanism (storage-opt vs the ulimit fallback) is decided
	// once per process. The first container creation pays the probe cost.
	storageProbeOnce sync.Once
	storageOptOK     bool

	// ── persistent per-conversation sandboxes (#213) ──
	// When PersistentREPL is set, TakePersistent(convID) keeps ONE sandbox alive
	// per conversation across turns (so the python kernel's variables/imports
	// survive). These are entirely separate from the warm slots above — a
	// persistent sandbox is never parked back in the warm queue, and is keyed by
	// conversation ID so it is NEVER shared across conversations (the isolation
	// invariant the warm pool's per-turn freshness also upholds).
	//
	// persistentMu guards persistent + persistentClosed. It is a SEPARATE mutex
	// from p.mu so the slow create/close paths here never contend with the warm
	// queue's hot path.
	persistentMu     sync.Mutex
	persistent       map[string]*persistentEntry
	persistentClosed bool
	persistentDone   chan struct{} // closed by Close to stop the idle reaper
}

// persistentEntry is one conversation's long-lived sandbox plus the bookkeeping
// the idle reaper and the cancelled-turn-overlap guard need. inUse is a borrow
// refcount: a turn increments it on TakePersistent and decrements it on the
// returned cleanup. The reaper closes an entry ONLY when inUse==0, so a long
// turn can never have its sandbox pulled out from under it; closeRequested lets
// a conversation-delete that races an in-flight turn defer the actual Close to
// the last borrow release.
type persistentEntry struct {
	sb             *Sandbox
	convID         string
	lastUsed       time.Time
	inUse          int
	closeRequested bool
}

// parkedSandbox is a warm sandbox plus the time it was parked, so the TTL keeper
// (#181) can reap containers that have sat idle past FLEET_SANDBOX_WARM_TTL —
// long-idle warm containers may have been OOM-killed or had their cgroup frozen,
// and handing one to a turn would fail the first tool call.
type parkedSandbox struct {
	sb       *Sandbox
	parkedAt time.Time
}

// PoolConfig holds the knobs for a Pool. Mode picks the backend; in
// production this is always ModeContainer (agent.go enforces it at
// boot). ModeHost remains as a test-only fixture.
type PoolConfig struct {
	Size int
	Mode Mode

	// BridgeScript is the embedded python_bridge.py contents, passed to
	// every host or container Sandbox we construct.
	BridgeScript []byte

	// Container holds the per-sandbox container settings (image, mounts,
	// caps). Required when Mode == ModeContainer.
	Container ContainerConfig

	// FillCtx is the parent context the warming goroutines run under.
	// Defaults to context.Background.
	FillCtx context.Context

	// WarmTTL bounds how long a warm sandbox may sit parked before it is reaped
	// and replaced (FLEET_SANDBOX_WARM_TTL). Zero disables TTL reaping (warm
	// containers live until taken or the pool is closed — the prior behaviour).
	// A background keeper reaps on a ticker; Take also skips an over-TTL slot.
	WarmTTL time.Duration

	// ── python REPL (#213) ──
	// PythonCellTimeout is the per-cell run_python ceiling stamped on every
	// sandbox this pool builds (FLEET_PYTHON_CELL_TIMEOUT). Zero disables it.
	PythonCellTimeout time.Duration
	// PersistentREPL enables the per-conversation persistent sandbox lifecycle
	// (FLEET_PYTHON_REPL_MODE=persistent). When false, TakePersistent degrades to
	// the per-turn Take (fresh sandbox + Close cleanup), so callers can route
	// through it unconditionally.
	PersistentREPL bool
	// PersistentIdleTTL bounds how long a persistent per-conversation sandbox may
	// sit idle (no run_python/bash call) before the reaper closes it
	// (FLEET_PYTHON_REPL_IDLE_TTL). Zero falls back to 30m.
	PersistentIdleTTL time.Duration
	// PersistentMaxSessions caps how many persistent sandboxes may be live at
	// once (FLEET_PYTHON_REPL_MAX); past it the least-recently-used idle session
	// is evicted. Zero disables the cap.
	PersistentMaxSessions int
}

// NewPool returns a Pool of the given size. Pass Size <= 0 to disable
// warming — Take will cold-start a sandbox every time. Call Close on
// process shutdown to reap remaining sandboxes.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.FillCtx == nil {
		cfg.FillCtx = context.Background()
	}
	p := &Pool{cfg: cfg, nowFn: time.Now}
	// Persistent per-conversation REPL (#213) is independent of the warm pool:
	// it works even with Size<=0 (every conversation cold-starts its first turn).
	if cfg.PersistentREPL {
		p.persistent = make(map[string]*persistentEntry)
		if p.cfg.PersistentIdleTTL <= 0 {
			p.cfg.PersistentIdleTTL = 30 * time.Minute
		}
		p.persistentDone = make(chan struct{})
		safe.Go("sandbox.pool.persistentKeeper", func() { p.persistentKeeper(p.persistentDone) })
	}
	if cfg.Size <= 0 {
		return p
	}
	p.slots = make(chan parkedSandbox, cfg.Size)
	go func() {
		defer safe.Recover("sandbox.pool.warm", nil)
		for i := 0; i < cfg.Size; i++ {
			p.fill()
		}
	}()
	// TTL keeper: reap warm containers that sit idle past WarmTTL so a long-idle
	// pool doesn't serve dead containers. Only runs when a TTL is configured.
	if cfg.WarmTTL > 0 {
		p.done = make(chan struct{})
		safe.Go("sandbox.pool.keeper", func() { p.keeper(p.done) })
	}
	return p
}

// now reads the pool's clock (time.Now in production; a fake in tests).
func (p *Pool) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

// stale reports whether a parked sandbox has sat idle past WarmTTL.
func (p *Pool) stale(ps parkedSandbox) bool {
	return p.cfg.WarmTTL > 0 && p.now().Sub(ps.parkedAt) > p.cfg.WarmTTL
}

// TakeContainer always returns a fresh container-mode sandbox with
// no-network enforced. Used by the lockdown path — lockdown chats
// force network-isolated containers regardless of the warm pool. Always
// cold-starts (no warm pool for forced-container turns); the latency
// cost is the price of opt-in lockdown mode.
//
// Returns ErrContainerUnavailable when the pool wasn't constructed
// with a container image (test/mock setups).
func (p *Pool) TakeContainer(ctx context.Context) (*Sandbox, func(), error) {
	return p.TakeContainerWithOverrides(ctx, ResourceOverride{}, true)
}

// ResourceOverride optionally overrides the pool's default cgroup ceilings for a
// single cold-started container (#205). An empty/zero field keeps the pool
// default. It carries pre-formatted podman-ready values so the sandbox package
// stays free of sched/task types.
type ResourceOverride struct {
	MemoryLimit string // e.g. "2048m"; "" = pool default
	CPULimit    string // e.g. "2.00";  "" = pool default
	PidsLimit   int    // e.g. 512;     0  = pool default
}

// applyTo returns cfg with this override's set (non-empty/non-zero) fields layered
// over the pool defaults (#205). The resulting MemoryLimit/CPULimit/PidsLimit are
// what containerImpl.start() emits verbatim as --memory / --memory-swap / --cpus /
// --pids-limit, so testing this pure merge proves the per-task flags.
func (ov ResourceOverride) applyTo(cfg ContainerConfig) ContainerConfig {
	if ov.MemoryLimit != "" {
		cfg.MemoryLimit = ov.MemoryLimit
	}
	if ov.CPULimit != "" {
		cfg.CPULimit = ov.CPULimit
	}
	if ov.PidsLimit > 0 {
		cfg.PidsLimit = ov.PidsLimit
	}
	return cfg
}

// TakeContainerWithOverrides cold-starts a fresh container like TakeContainer,
// but applies per-task resource overrides (#205) and lets the caller choose the
// network posture: noNetwork=true seals egress (--network=none, the lockdown
// boundary and the default for sealed scheduled runs), false leaves the default
// rootless slirp4netns egress (matching the warm pool's newSandbox). Always
// cold-starts — a warm pooled container is already running with the pool's
// ceilings, so per-task limits inherently require a fresh container.
//
// Returns ErrContainerUnavailable when the pool wasn't constructed with a
// container image (test/mock setups), exactly like TakeContainer.
func (p *Pool) TakeContainerWithOverrides(ctx context.Context, ov ResourceOverride, noNetwork bool) (*Sandbox, func(), error) {
	if p == nil {
		return nil, func() {}, ErrContainerUnavailable
	}
	if p.cfg.Container.Image == "" {
		return nil, func() {}, ErrContainerUnavailable
	}
	cfg := p.cfg.Container
	cfg.BridgeScript = p.cfg.BridgeScript
	cfg.StorageOptSupported = p.storageOptSupported(ctx)
	// Network sealing is enforced HERE rather than upstream so the lockdown
	// contract is impossible to bypass via a bad caller.
	cfg.NoNetwork = noNetwork
	// Per-task overrides (#205) tighten/raise the cgroup caps within the operator
	// ceiling already validated at task creation. An empty/zero field leaves the
	// pool default (applyContainerDefaults fills it in NewContainer).
	cfg = ov.applyTo(cfg)
	// See newSandbox below for why we resolve the start timeout here
	// rather than reading it raw from cfg.
	startCtx, cancel := context.WithTimeout(ctx, resolveStartTimeout(cfg)+5*time.Second)
	defer cancel()
	sb, err := NewContainer(startCtx, cfg)
	if err != nil {
		return nil, func() {}, err
	}
	sb.SetPythonCellTimeout(p.cfg.PythonCellTimeout)
	return sb, sb.Close, nil
}

// Take pulls a warm sandbox or constructs a fresh one if none are
// ready. Returns (sandbox, cleanup) — call cleanup when the turn ends.
//
// Implementation note: the cleanup is just sandbox.Close, surfaced as a
// separate value to mirror the legacy KernelPool API and to make it
// harder to forget.
func (p *Pool) Take() (*Sandbox, func(), error) {
	if p == nil || p.cfg.Size <= 0 || p.slots == nil {
		sb, err := p.newSandbox(p.cfg.FillCtx)
		if err != nil {
			return nil, func() {}, err
		}
		return sb, sb.Close, nil
	}
	for {
		select {
		case ps, ok := <-p.slots:
			if !ok || ps.sb == nil {
				// Channel closed (pool shutting down) — cold-start so the caller
				// still gets a usable sandbox rather than a nil.
				return p.coldStart()
			}
			// Every received slot — taken or reaped — gets one async refill so the
			// pool returns to depth.
			safe.Go("sandbox.pool.fill", p.fill)
			if p.stale(ps) {
				// Over-TTL: this warm container may be dead. Reap it and try the
				// next parked one rather than hand out a likely-broken sandbox.
				ps.sb.Close()
				continue
			}
			return ps.sb, ps.sb.Close, nil
		default:
			// Pool empty (or only-stale just drained) — cold-start.
			return p.coldStart()
		}
	}
}

// coldStart constructs a fresh sandbox on the caller's goroutine (the no-warm-slot
// path of Take).
func (p *Pool) coldStart() (*Sandbox, func(), error) {
	sb, err := p.newSandbox(p.cfg.FillCtx)
	if err != nil {
		return nil, func() {}, err
	}
	return sb, sb.Close, nil
}

// Stats reports the configured warm-pool size and how many sandboxes are
// currently parked and ready (#301 health summary). A nil or unwarmed pool
// (Size<=0, cold-start every Take) reports (0, 0).
func (p *Pool) Stats() (size, available int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.slots == nil {
		return 0, 0
	}
	return p.cfg.Size, len(p.slots)
}

// Close reaps every remaining sandbox. Safe to call on a nil pool or to
// call multiple times.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.done != nil {
		close(p.done) // stop the TTL keeper before draining
	}
	if p.slots != nil {
		close(p.slots)
	}
	p.mu.Unlock()

	if p.slots != nil {
		for ps := range p.slots {
			if ps.sb != nil {
				ps.sb.Close()
			}
		}
	}

	// Drain persistent per-conversation sandboxes (#213). Stop the idle reaper,
	// snapshot the live entries, clear the map, then Close outside the lock (the
	// podman teardown is slow). closeRequested entries with in-flight borrows are
	// closed here too: shutdown overrides the defer-to-last-release rule.
	p.persistentMu.Lock()
	var persistent []*persistentEntry
	if !p.persistentClosed {
		p.persistentClosed = true
		if p.persistentDone != nil {
			close(p.persistentDone)
		}
		for _, e := range p.persistent {
			persistent = append(persistent, e)
		}
		p.persistent = nil
	}
	p.persistentMu.Unlock()
	if len(persistent) > 0 {
		log.Printf("sandbox.Pool.Close: closing %d persistent conversation sandbox(es)", len(persistent))
	}
	for _, e := range persistent {
		e.sb.Close()
	}
}

// fill spawns one sandbox and parks it. Errors are logged because the
// pool is a best-effort optimization; Take will cold-start if needed.
//
// The non-blocking send happens under p.mu so we can't race with
// Close() closing the channel — closing-while-sending is a data race
// even when the resulting panic is recovered.
func (p *Pool) fill() {
	sb, err := p.newSandbox(p.cfg.FillCtx)
	if err != nil {
		log.Printf("sandbox.Pool.fill: %v", err)
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		sb.Close()
		return
	}
	select {
	case p.slots <- parkedSandbox{sb: sb, parkedAt: p.now()}:
		p.mu.Unlock()
	default:
		// Pool full (concurrent fill won the race). Drop the spare.
		p.mu.Unlock()
		sb.Close()
	}
}

// keeper reaps warm sandboxes that have sat idle past WarmTTL, on a ticker, so a
// pool that is idle for a long stretch (no Take to lazily reap on) doesn't keep
// serving stale containers. It exits when done is closed (by Close).
func (p *Pool) keeper(done chan struct{}) {
	interval := p.cfg.WarmTTL / 2
	if interval <= 0 || interval > 30*time.Second {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			p.reapStale()
		}
	}
}

// reapStale drains the warm queue, re-parks the still-fresh sandboxes, and closes
// the over-TTL ones (replacing each with a fresh fill). Fresh sandboxes are
// re-parked BEFORE the slow container teardown so the window in which they are
// out of the pool is minimal. Safe against a concurrent Take (each parked entry
// is received by exactly one of them) and Close (re-park is guarded by p.closed).
func (p *Pool) reapStale() {
	if p.slots == nil {
		return
	}
	var fresh, stale []parkedSandbox
	for drained := false; !drained; {
		select {
		case ps, ok := <-p.slots:
			if !ok {
				// Channel closed by Close mid-tick. Stop draining; Close owns the
				// teardown of anything still in the channel.
				drained = true
				break
			}
			if ps.sb == nil {
				continue
			}
			if p.stale(ps) {
				stale = append(stale, ps)
			} else {
				fresh = append(fresh, ps)
			}
		default:
			drained = true
		}
	}
	if len(fresh) == 0 && len(stale) == 0 {
		return
	}

	// Re-park the fresh ones (fast), coordinated with Close exactly like fill.
	p.mu.Lock()
	closing := p.closed
	if !closing {
		for _, ps := range fresh {
			select {
			case p.slots <- ps:
			default:
				// Slot unexpectedly full (a concurrent fill refilled it); drop the
				// spare rather than block. Closed outside the lock below.
				stale = append(stale, ps)
			}
		}
		fresh = nil
	}
	p.mu.Unlock()

	// Pool is closing: don't re-park; Close drains what's in the channel, we close
	// what we still hold. Either way, tear down the reaped/dropped containers
	// (slow podman stop/rm) outside the lock, and replace each reaped slot.
	toClose := stale
	if closing {
		toClose = append(toClose, fresh...)
	}
	for _, ps := range toClose {
		ps.sb.Close()
	}
	if !closing {
		for range stale {
			safe.Go("sandbox.pool.fill", p.fill)
		}
	}
}

// storageOptSupported probes once (cached) whether the host's storage driver
// supports `--storage-opt size` disk quotas, logging which quota mechanism the
// sandbox will use. Thread-safe; the first container creation pays the probe.
func (p *Pool) storageOptSupported(ctx context.Context) bool {
	p.storageProbeOnce.Do(func() {
		gb := effectiveDiskGB(p.cfg.Container.DiskLimitGB)
		if gb <= 0 {
			// Quota disabled: no flag either way, so skip the probe container.
			log.Printf("sandbox disk quota: DISABLED (FLEET_SANDBOX_DISK_GB<=0) — the host disk is unprotected from runaway sandbox writes")
			return
		}
		p.storageOptOK = ProbeStorageOptSupport(ctx, p.cfg.Container.PodmanBinary, p.cfg.Container.Image)
		if p.storageOptOK {
			log.Printf("sandbox disk quota: storage-opt size=%dg — writable layer hard-capped (total usage bounded)", gb)
		} else {
			log.Printf("sandbox disk quota: storage-opt unsupported on this storage driver; falling back to ulimit fsize=%dGiB — caps any single file but NOT total disk use (use overlay+xfs(pquota)/btrfs for a hard cap)", gb)
		}
	})
	return p.storageOptOK
}

func (p *Pool) newSandbox(ctx context.Context) (*Sandbox, error) {
	switch p.cfg.Mode {
	case ModeContainer:
		cfg := p.cfg.Container
		cfg.BridgeScript = p.cfg.BridgeScript
		cfg.StorageOptSupported = p.storageOptSupported(ctx)
		// resolveStartTimeout applies the same default NewContainer would
		// apply internally. Without this, the OUTER context timeout is
		// `0+5s = 5s` when StartTimeout isn't set explicitly, which
		// cancels podman before its first-run idmapped-layer chown
		// finishes — that chown takes ~12s on a fresh sandbox image
		// pull. The inner NewContainer would happily wait 30s, but the
		// outer 5s context cancels before it gets there. Symptom:
		// "first message after deploy fails, second works fine"
		// (because by the time the second message lands, the warm pool
		// has finished filling against the now-cached chowned layer).
		startCtx, cancel := context.WithTimeout(ctx, resolveStartTimeout(cfg)+5*time.Second)
		defer cancel()
		sb, err := NewContainer(startCtx, cfg)
		if err != nil {
			return nil, err
		}
		sb.SetPythonCellTimeout(p.cfg.PythonCellTimeout)
		return sb, nil
	case ModeHost:
		// Test-only fixture path. agent.go forbids ModeHost in
		// production; this branch only fires when sandbox_test.go
		// constructs a pool directly with ModeHost. newHostSandbox is the
		// real host executor only with -tags fleet_host_executor; a release
		// build's stub returns an error here (#159).
		sb, err := newHostSandbox(p.cfg.BridgeScript)
		if err != nil {
			return nil, err
		}
		sb.SetPythonCellTimeout(p.cfg.PythonCellTimeout)
		return sb, nil
	default:
		return nil, ErrContainerUnavailable
	}
}
