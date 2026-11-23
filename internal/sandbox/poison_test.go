package sandbox

// #796 unit coverage for the poison/retire mechanism, independent of any real
// backend: a poisoned sandbox refuses work fail-closed, and the persistent
// pool retires a poisoned entry at both claim and release so no later turn can
// share a container with a cancelled command's stragglers.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// poisonableImpl is a backend stub whose poisoned() state the test controls.
type poisonableImpl struct {
	poison atomic.Bool
	closed atomic.Bool
}

func (p *poisonableImpl) runBash(context.Context, BashRequest) (BashResult, error) {
	return BashResult{}, nil
}
func (p *poisonableImpl) runPython(context.Context, PythonRequest) (PythonResult, error) {
	return PythonResult{Status: "success"}, nil
}
func (p *poisonableImpl) runFileOp(context.Context, FileOpRequest) (FileOpResult, error) {
	return FileOpResult{}, nil
}
func (p *poisonableImpl) bindFileOpRoot(context.Context, string) (FileOpRootIdentity, error) {
	return FileOpRootIdentity{Dev: 1, Ino: 1}, nil
}
func (p *poisonableImpl) poisoned() bool { return p.poison.Load() }
func (p *poisonableImpl) close()         { p.closed.Store(true) }

func TestSandbox_PoisonedRefusesWork(t *testing.T) {
	pi := &poisonableImpl{}
	sb := &Sandbox{mode: ModeContainer, impl: pi}
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); err != nil {
		t.Fatalf("healthy sandbox: %v", err)
	}
	pi.poison.Store(true)
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("RunBash on poisoned sandbox = %v, want ErrPoisoned (fail closed)", err)
	}
	if _, err := sb.RunPython(context.Background(), PythonRequest{Code: "1"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("RunPython on poisoned sandbox = %v, want ErrPoisoned", err)
	}
}

func TestTakePersistent_PoisonedEntryIsRetiredAtRelease(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	pi := &poisonableImpl{}
	entry := &persistentEntry{sb: &Sandbox{mode: ModeContainer, impl: pi}, convID: "conv-poison"}
	p.persistentMu.Lock()
	p.persistent["conv-poison"] = entry
	p.persistentMu.Unlock()

	sb, release, err := p.TakePersistent(context.Background(), "conv-poison")
	if err != nil {
		t.Fatalf("TakePersistent: %v", err)
	}
	if sb != entry.sb {
		t.Fatal("expected the seeded healthy entry to be reused")
	}
	pi.poison.Store(true) // the turn's bash was cancelled → container killed → poisoned
	release()

	if !pi.closed.Load() {
		t.Fatal("releasing a poisoned persistent sandbox must close it")
	}
	p.persistentMu.Lock()
	_, still := p.persistent["conv-poison"]
	p.persistentMu.Unlock()
	if still {
		t.Fatal("poisoned entry must leave the pool so the next turn gets a fresh sandbox")
	}
}

func TestTakePersistent_PoisonedEntryIsRetiredAtClaim(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	pi := &poisonableImpl{}
	pi.poison.Store(true)
	entry := &persistentEntry{sb: &Sandbox{mode: ModeContainer, impl: pi}, convID: "conv-poison"}
	p.persistentMu.Lock()
	p.persistent["conv-poison"] = entry
	p.persistentMu.Unlock()

	sb, release, err := p.TakePersistent(context.Background(), "conv-poison")
	if err != nil {
		t.Fatalf("TakePersistent: %v", err)
	}
	defer release()
	if sb == entry.sb {
		t.Fatal("a poisoned sandbox must never be lent to another turn")
	}
	if !pi.closed.Load() {
		t.Fatal("the poisoned sandbox must be closed on retirement")
	}
}

// A sandbox the pool handed out knows how to rebuild itself, so one timed-out
// command no longer costs the rest of the run: the next call of any kind runs
// on a fresh backend, and the poisoned one is closed first.
func TestSandbox_PoisonedBackendIsReplacedWithinTheRun(t *testing.T) {
	old := &poisonableImpl{}
	fresh := &poisonableImpl{}
	builds := 0
	sb := &Sandbox{mode: ModeContainer, impl: old}
	sb.setReplace(func(context.Context) (*Sandbox, error) {
		builds++
		if !old.closed.Load() {
			t.Error("the poisoned backend must be closed before its replacement starts")
		}
		return &Sandbox{mode: ModeContainer, impl: fresh}, nil
	})
	ctx := context.Background()
	if err := sb.BindFileOpRoot(ctx, "/workspace/conv"); err != nil {
		t.Fatalf("BindFileOpRoot: %v", err)
	}

	old.poison.Store(true)
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Root: "/workspace/conv", Path: "/workspace/conv/a.csv"}); err != nil {
		t.Fatalf("RunFileOp after a poisoned command = %v, want it to run on a replacement", err)
	}
	if _, err := sb.RunPython(ctx, PythonRequest{Code: "1"}); err != nil {
		t.Fatalf("RunPython after replacement: %v", err)
	}
	if _, err := sb.RunBash(ctx, BashRequest{Command: "true"}); err != nil {
		t.Fatalf("RunBash after replacement: %v", err)
	}
	if builds != 1 {
		t.Fatalf("replacement built %d times, want once for one poisoning", builds)
	}
	if sb.Poisoned() {
		t.Fatal("a sandbox running on its replacement must not read as poisoned")
	}
	sb.Close()
	if !fresh.closed.Load() {
		t.Fatal("Close must close the replacement backend")
	}
}

func TestSandbox_ParallelCallsShareOneReplacement(t *testing.T) {
	old := &poisonableImpl{}
	var builds atomic.Int32
	sb := &Sandbox{mode: ModeContainer, impl: old}
	sb.setReplace(func(context.Context) (*Sandbox, error) {
		builds.Add(1)
		time.Sleep(10 * time.Millisecond)
		return &Sandbox{mode: ModeContainer, impl: &poisonableImpl{}}, nil
	})
	old.poison.Store(true)

	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := sb.RunBash(context.Background(), BashRequest{Command: "true"})
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("parallel RunBash on a poisoned sandbox: %v", err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("replacement built %d times for one poisoning, want 1", got)
	}
}

// Every way a replacement can go wrong leaves the call where it was before
// replacement existed: ErrPoisoned, and no backend the checks did not pass.
func TestSandbox_ReplacementFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("replacement cannot be started", func(t *testing.T) {
		old := &poisonableImpl{}
		sb := &Sandbox{mode: ModeContainer, impl: old}
		sb.setReplace(func(context.Context) (*Sandbox, error) { return nil, errors.New("podman run failed") })
		old.poison.Store(true)
		if _, err := sb.RunBash(ctx, BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("RunBash = %v, want ErrPoisoned", err)
		}
		if !sb.Poisoned() {
			t.Fatal("the sandbox must stay poisoned so its owner retires it")
		}
	})

	t.Run("replacement mounts a different directory", func(t *testing.T) {
		old := &poisonableImpl{}
		other := &otherRootImpl{}
		sb := &Sandbox{mode: ModeContainer, impl: old}
		sb.setReplace(func(context.Context) (*Sandbox, error) {
			return &Sandbox{mode: ModeContainer, impl: other}, nil
		})
		if err := sb.BindFileOpRoot(ctx, "/workspace/conv"); err != nil {
			t.Fatalf("BindFileOpRoot: %v", err)
		}
		old.poison.Store(true)
		if _, err := sb.RunBash(ctx, BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("RunBash = %v, want ErrPoisoned when the replacement's root identity differs", err)
		}
		if !other.closed.Load() {
			t.Fatal("a refused replacement must be closed")
		}
	})

	t.Run("replacement fails the take path's check", func(t *testing.T) {
		old := &poisonableImpl{}
		fresh := &poisonableImpl{}
		sb := &Sandbox{mode: ModeContainer, impl: old}
		sb.setReplace(func(context.Context) (*Sandbox, error) {
			return &Sandbox{mode: ModeContainer, impl: fresh}, nil
		})
		sb.checkReplacement(func(context.Context, *Sandbox) error { return errors.New("scoped mount changed") })
		old.poison.Store(true)
		if _, err := sb.RunPython(ctx, PythonRequest{Code: "1"}); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("RunPython = %v, want ErrPoisoned", err)
		}
		if !fresh.closed.Load() {
			t.Fatal("a replacement that fails its check must be closed")
		}
	})

	t.Run("caller's context is done", func(t *testing.T) {
		old := &poisonableImpl{}
		sb := &Sandbox{mode: ModeContainer, impl: old}
		sb.setReplace(func(context.Context) (*Sandbox, error) {
			t.Error("a cancelled run must not start a container")
			return nil, errors.New("unreachable")
		})
		old.poison.Store(true)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := sb.RunBash(cancelled, BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("RunBash = %v, want ErrPoisoned", err)
		}
	})

	t.Run("bounded", func(t *testing.T) {
		cur := &poisonableImpl{}
		sb := &Sandbox{mode: ModeContainer, impl: cur}
		sb.setReplace(func(context.Context) (*Sandbox, error) {
			cur = &poisonableImpl{}
			return &Sandbox{mode: ModeContainer, impl: cur}, nil
		})
		for i := range maxBackendReplacements {
			cur.poison.Store(true)
			if _, err := sb.RunBash(ctx, BashRequest{Command: "true"}); err != nil {
				t.Fatalf("replacement %d: %v", i+1, err)
			}
		}
		cur.poison.Store(true)
		if _, err := sb.RunBash(ctx, BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("RunBash past the replacement bound = %v, want ErrPoisoned", err)
		}
	})
}

// otherRootImpl is a healthy backend whose bound root is a different
// directory than poisonableImpl's.
type otherRootImpl struct{ poisonableImpl }

func (o *otherRootImpl) bindFileOpRoot(context.Context, string) (FileOpRootIdentity, error) {
	return FileOpRootIdentity{Dev: 9, Ino: 9}, nil
}
