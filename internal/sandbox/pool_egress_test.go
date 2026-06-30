package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestTakeContainerWithEgressFailsClosed verifies the security-critical
// guarantee: an allowlisted request never silently becomes open egress. Both
// paths short-circuit before any container is started, so no podman is needed.
func TestTakeContainerWithEgressFailsClosed(t *testing.T) {
	// No image (test/mock pool) → ErrContainerUnavailable, like TakeContainer.
	p := NewPool(PoolConfig{})
	if _, _, err := p.TakeContainerWithEgress(context.Background(), ResourceOverride{}, []string{"pypi.org"}); !errors.Is(err, ErrContainerUnavailable) {
		t.Fatalf("no image: err = %v, want ErrContainerUnavailable", err)
	}

	// Image set but NO egress proxy configured → must FAIL CLOSED with a distinct
	// error, never proceed to start an open-egress container.
	withImage := NewPool(PoolConfig{Container: ContainerConfig{Image: "example/img"}})
	_, _, err := withImage.TakeContainerWithEgress(context.Background(), ResourceOverride{}, []string{"pypi.org"})
	if err == nil {
		t.Fatal("allowlisted request with no proxy must error (fail closed), got nil")
	}
	if errors.Is(err, ErrContainerUnavailable) {
		t.Fatalf("want a distinct fail-closed error, got ErrContainerUnavailable")
	}
	if !strings.Contains(err.Error(), "egress proxy") {
		t.Errorf("fail-closed error = %q, want it to mention the missing egress proxy", err)
	}
}
