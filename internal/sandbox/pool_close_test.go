package sandbox

import (
	"context"
	"errors"
	"testing"
)

func TestNilPoolTakeFailsClosed(t *testing.T) {
	var p *Pool
	tests := []struct {
		name string
		take func() (*Sandbox, func(), error)
	}{
		{name: "turn", take: p.Take},
		{name: "persistent", take: func() (*Sandbox, func(), error) { return p.TakePersistent("conv") }},
		{name: "container", take: func() (*Sandbox, func(), error) { return p.TakeContainer(context.Background()) }},
		{name: "container overrides", take: func() (*Sandbox, func(), error) {
			return p.TakeContainerWithOverrides(context.Background(), ResourceOverride{}, true)
		}},
		{name: "allowlisted container", take: func() (*Sandbox, func(), error) {
			return p.TakeContainerWithEgress(context.Background(), ResourceOverride{}, []string{"example.com"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb, cleanup, err := tt.take()
			if !errors.Is(err, ErrContainerUnavailable) {
				t.Fatalf("error = %v, want ErrContainerUnavailable", err)
			}
			if sb != nil {
				t.Fatalf("sandbox = %v, want nil", sb)
			}
			cleanup() // Error paths retain the non-nil cleanup contract.
		})
	}
}

func TestPoolTakeAfterCloseFailsClosed(t *testing.T) {
	p := NewPool(PoolConfig{
		Mode: ModeHost,
		Container: ContainerConfig{
			Image: "unused-because-the-closed-gate-runs-first",
		},
		EgressProxy: NewEgressProxy(),
	})
	p.Close()

	tests := []struct {
		name string
		take func() (*Sandbox, func(), error)
	}{
		{name: "turn", take: p.Take},
		{name: "persistent", take: func() (*Sandbox, func(), error) { return p.TakePersistent("conv") }},
		{name: "container", take: func() (*Sandbox, func(), error) { return p.TakeContainer(context.Background()) }},
		{name: "container overrides", take: func() (*Sandbox, func(), error) {
			return p.TakeContainerWithOverrides(context.Background(), ResourceOverride{}, true)
		}},
		{name: "allowlisted container", take: func() (*Sandbox, func(), error) {
			return p.TakeContainerWithEgress(context.Background(), ResourceOverride{}, []string{"example.com"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb, cleanup, err := tt.take()
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("error = %v, want ErrClosed", err)
			}
			if sb != nil {
				t.Fatalf("sandbox = %v, want nil", sb)
			}
			cleanup()
		})
	}
}
