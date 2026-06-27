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
//   - cleanup() closes the sandbox; per-turn isolation is preserved.
//     Pool members are never recycled across turns — fresh containers
//     for fresh turns. This avoids the OpenAI-2024-style cross-conv
//     leak where a "warm" sandbox carried files from the previous
//     conversation into the next.
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
	slots  chan *Sandbox
	closed bool

	// storageProbeOnce caches the one-time --storage-opt support probe (#216) so
	// the disk-quota mechanism (storage-opt vs the ulimit fallback) is decided
	// once per process. The first container creation pays the probe cost.
	storageProbeOnce sync.Once
	storageOptOK     bool
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
}

// NewPool returns a Pool of the given size. Pass Size <= 0 to disable
// warming — Take will cold-start a sandbox every time. Call Close on
// process shutdown to reap remaining sandboxes.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.FillCtx == nil {
		cfg.FillCtx = context.Background()
	}
	p := &Pool{cfg: cfg}
	if cfg.Size <= 0 {
		return p
	}
	p.slots = make(chan *Sandbox, cfg.Size)
	go func() {
		defer safe.Recover("sandbox.pool.warm", nil)
		for i := 0; i < cfg.Size; i++ {
			p.fill()
		}
	}()
	return p
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
	if p == nil {
		return nil, func() {}, ErrContainerUnavailable
	}
	if p.cfg.Container.Image == "" {
		return nil, func() {}, ErrContainerUnavailable
	}
	cfg := p.cfg.Container
	cfg.BridgeScript = p.cfg.BridgeScript
	cfg.StorageOptSupported = p.storageOptSupported(ctx)
	// Lockdown's distinguishing security guarantee: no network egress
	// from inside bash/run_python. Non-lockdown chats (Pool.Take ->
	// newSandbox) inherit the default rootless slirp4netns so routine
	// flows like `pip install` or curling a URL the user mentions just
	// work. The lockdown gate is enforced HERE rather than upstream so
	// the contract is impossible to bypass via a bad caller.
	cfg.NoNetwork = true
	// See newSandbox below for why we resolve the start timeout here
	// rather than reading it raw from cfg.
	startCtx, cancel := context.WithTimeout(ctx, resolveStartTimeout(cfg)+5*time.Second)
	defer cancel()
	sb, err := NewContainer(startCtx, cfg)
	if err != nil {
		return nil, func() {}, err
	}
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
	select {
	case sb := <-p.slots:
		// Refill async so concurrent turns don't starve.
		safe.Go("sandbox.pool.fill", p.fill)
		return sb, sb.Close, nil
	default:
		// Pool empty — cold-start. Replenish anyway.
		safe.Go("sandbox.pool.fill", p.fill)
		sb, err := p.newSandbox(p.cfg.FillCtx)
		if err != nil {
			return nil, func() {}, err
		}
		return sb, sb.Close, nil
	}
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
	if p.slots != nil {
		close(p.slots)
	}
	p.mu.Unlock()

	if p.slots != nil {
		for sb := range p.slots {
			if sb != nil {
				sb.Close()
			}
		}
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
	case p.slots <- sb:
		p.mu.Unlock()
	default:
		// Pool full (concurrent fill won the race). Drop the spare.
		p.mu.Unlock()
		sb.Close()
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
		return NewContainer(startCtx, cfg)
	case ModeHost:
		// Test-only fixture path. agent.go forbids ModeHost in
		// production; this branch only fires when sandbox_test.go
		// constructs a pool directly with ModeHost. newHostSandbox is the
		// real host executor only with -tags fleet_host_executor; a release
		// build's stub returns an error here (#159).
		return newHostSandbox(p.cfg.BridgeScript)
	default:
		return nil, ErrContainerUnavailable
	}
}
