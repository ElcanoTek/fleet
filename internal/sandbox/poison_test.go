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
func (p *poisonableImpl) resourceUsage() (ResourceUsageSummary, bool) {
	return ResourceUsageSummary{}, false
}
func (p *poisonableImpl) runFileOp(context.Context, FileOpRequest) (FileOpResult, error) {
	return FileOpResult{}, nil
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

	sb, release, err := p.TakePersistent("conv-poison")
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

	sb, release, err := p.TakePersistent("conv-poison")
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
