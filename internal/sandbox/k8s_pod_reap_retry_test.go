// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A pod delete fails for reasons that have nothing to do with the pod — an
// apiserver restart, a network blip, throttling — and the pod then keeps its
// Guaranteed CPU/memory reservation. Nothing else reclaims it: the boot sweep
// deliberately skips pods carrying THIS incarnation's label, so it cannot
// collect fleet's own leftovers, and the owning control-plane pod is alive so
// the garbage collector will not either (#1264).
//
// Measured before this retry existed: an 83-second apiserver outage stranded
// two sandbox pods, and they were still Running 90 seconds after the cluster
// had recovered.
func TestFailedPodDeleteIsRetried(t *testing.T) {
	restore := shortenReapInterval(t)
	defer restore()

	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}

	// The next two deletes fail, exactly as they did against the real cluster.
	fake.mu.Lock()
	fake.deleteFailures = 2
	fake.mu.Unlock()

	sb.Close()

	// Close's own delete failed, so the pod is still there …
	fake.mu.Lock()
	live := len(fake.pods)
	fake.mu.Unlock()
	if live != 1 {
		t.Fatalf("expected the pod to survive a failed delete, got %d live", live)
	}

	// … and the background retry must reclaim it once the apiserver answers.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fake.mu.Lock()
		live = len(fake.pods)
		fake.mu.Unlock()
		if live == 0 {
			return // reclaimed
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retry never reclaimed the pod: %d still live", live)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The queue must not grow without bound, and a drain goroutine must not
// outlive the work — an installation that never fails a delete should never
// carry one.
func TestReapQueueIsBoundedAndSelfTerminating(t *testing.T) {
	restore := shortenReapInterval(t)
	defer restore()

	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})

	for i := 0; i < podReapMaxPending+10; i++ {
		backend.scheduleReap(podNameForTest(i))
	}
	backend.reapMu.Lock()
	pending := len(backend.reapPending)
	backend.reapMu.Unlock()
	if pending > podReapMaxPending {
		t.Errorf("queue grew past its bound: %d > %d", pending, podReapMaxPending)
	}

	// Every one of those pods is absent, so the deletes come back NotFound —
	// which counts as reclaimed — and the goroutine should retire itself.
	deadline := time.Now().Add(5 * time.Second)
	for {
		backend.reapMu.Lock()
		pending, running := len(backend.reapPending), backend.reapRunning
		backend.reapMu.Unlock()
		if pending == 0 && !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain goroutine did not retire: pending=%d running=%v", pending, running)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func shortenReapInterval(t *testing.T) func() {
	t.Helper()
	oldInterval, oldMax := podReapInterval, podReapMaxInterval
	podReapInterval, podReapMaxInterval = 10*time.Millisecond, 20*time.Millisecond
	return func() { podReapInterval, podReapMaxInterval = oldInterval, oldMax }
}

func podNameForTest(i int) string {
	return fmt.Sprintf("fleet-sandbox-absent-%d", i)
}
