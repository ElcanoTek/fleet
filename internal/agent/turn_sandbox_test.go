package agent

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// Interactive-turn sandbox posture (#211 chat egress). Load-bearing: the
// fleet-wide network mode is honored for ordinary chat turns exactly as it is
// for scheduled tasks — lockdown seals, allowlisted filters through the proxy,
// open keeps the persistent/warm path — and a containing mode takes precedence
// over persistent REPL. Before this, chat turns silently got open egress.

type fakeTurnTaker struct {
	mode      string
	allowlist []string
	unavail   bool // container takes report ErrContainerUnavailable (host/mock)

	tookContainer  bool
	tookEgress     bool
	gotEgressList  []string
	tookPersistent bool
	tookWarm       bool
}

func (f *fakeTurnTaker) EgressDefault() (string, []string) { return f.mode, f.allowlist }

func (f *fakeTurnTaker) TakeContainer(context.Context) (*sandbox.Sandbox, func(), error) {
	f.tookContainer = true
	if f.unavail {
		return nil, func() {}, sandbox.ErrContainerUnavailable
	}
	return nil, func() {}, nil
}

func (f *fakeTurnTaker) TakeContainerWithEgress(_ context.Context, _ sandbox.ResourceOverride, allowlist []string) (*sandbox.Sandbox, func(), error) {
	f.tookEgress = true
	f.gotEgressList = allowlist
	if f.unavail {
		return nil, func() {}, sandbox.ErrContainerUnavailable
	}
	return nil, func() {}, nil
}

func (f *fakeTurnTaker) TakePersistent(string) (*sandbox.Sandbox, func(), error) {
	f.tookPersistent = true
	return nil, func() {}, nil
}

func (f *fakeTurnTaker) Take() (*sandbox.Sandbox, func(), error) {
	f.tookWarm = true
	return nil, func() {}, nil
}

func TestTakeTurnSandboxPosture(t *testing.T) {
	ctx := context.Background()
	base := turnSandboxPosture{lockdownAvailable: true}

	t.Run("open mode, persistent → persistent borrow", func(t *testing.T) {
		f := &fakeTurnTaker{mode: sandbox.NetworkModeOpen}
		p := base
		p.persistent = true
		p.convID = "c1"
		if _, _, err := takeTurnSandboxFrom(ctx, f, p); err != nil {
			t.Fatal(err)
		}
		if !f.tookPersistent || f.tookEgress || f.tookContainer {
			t.Errorf("open+persistent should borrow the persistent sandbox: %+v", f)
		}
	})

	t.Run("open mode, non-persistent → warm take", func(t *testing.T) {
		f := &fakeTurnTaker{mode: ""}
		if _, _, err := takeTurnSandboxFrom(ctx, f, base); err != nil {
			t.Fatal(err)
		}
		if !f.tookWarm || f.tookEgress || f.tookContainer {
			t.Errorf("open+non-persistent should warm-take: %+v", f)
		}
	})

	t.Run("fleet-wide lockdown seals every turn", func(t *testing.T) {
		f := &fakeTurnTaker{mode: sandbox.NetworkModeLockdown}
		p := base
		p.persistent = true // persistence must NOT win over a sealing mode
		p.convID = "c1"
		if _, _, err := takeTurnSandboxFrom(ctx, f, p); err != nil {
			t.Fatal(err)
		}
		if !f.tookContainer || f.tookPersistent || f.tookEgress {
			t.Errorf("lockdown mode must seal (TakeContainer), not persist: %+v", f)
		}
	})

	t.Run("allowlisted mode filters egress, precedes persistence", func(t *testing.T) {
		f := &fakeTurnTaker{mode: sandbox.NetworkModeAllowlisted, allowlist: []string{"api.example.com"}}
		p := base
		p.persistent = true
		p.convID = "c1"
		if _, _, err := takeTurnSandboxFrom(ctx, f, p); err != nil {
			t.Fatal(err)
		}
		if !f.tookEgress || f.tookPersistent {
			t.Errorf("allowlisted must filter egress, not persist: %+v", f)
		}
		if len(f.gotEgressList) != 1 || f.gotEgressList[0] != "api.example.com" {
			t.Errorf("egress allowlist not passed through: %v", f.gotEgressList)
		}
	})

	t.Run("per-conversation lockdown seals even in open mode", func(t *testing.T) {
		f := &fakeTurnTaker{mode: sandbox.NetworkModeOpen}
		p := base
		p.lockdown = true
		if _, _, err := takeTurnSandboxFrom(ctx, f, p); err != nil {
			t.Fatal(err)
		}
		if !f.tookContainer {
			t.Errorf("per-conversation lockdown must seal: %+v", f)
		}
	})

	t.Run("lockdown chat without a sandbox image errors", func(t *testing.T) {
		f := &fakeTurnTaker{mode: sandbox.NetworkModeOpen}
		p := base
		p.lockdown = true
		p.lockdownAvailable = false
		if _, _, err := takeTurnSandboxFrom(ctx, f, p); err == nil {
			t.Error("lockdown without an image should error")
		}
	})

	t.Run("sealing degrades to host take when no container backend", func(t *testing.T) {
		f := &fakeTurnTaker{mode: sandbox.NetworkModeLockdown, unavail: true}
		if _, _, err := takeTurnSandboxFrom(ctx, f, base); err != nil {
			t.Fatal(err)
		}
		if !f.tookWarm {
			t.Errorf("host/mock pool should fall back to warm take: %+v", f)
		}
	})

	t.Run("allowlisted degrades to host take when no container backend", func(t *testing.T) {
		f := &fakeTurnTaker{mode: sandbox.NetworkModeAllowlisted, unavail: true}
		if _, _, err := takeTurnSandboxFrom(ctx, f, base); err != nil {
			t.Fatal(err)
		}
		if !f.tookWarm {
			t.Errorf("host/mock pool should fall back to warm take: %+v", f)
		}
	})
}
