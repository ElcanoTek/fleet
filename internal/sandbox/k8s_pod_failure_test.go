// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"strings"
	"testing"
)

// A kubelet eviction and an OOM kill both reach a turn as a dead exec stream
// and nothing else, so the reason has to be fetched from the pod's own status
// (#1264). Without it the caller sees `bridge closed unexpectedly: EOF` and
// guesses — during validation an emptyDir eviction was reported to the user as
// an OOM kill, confidently and wrongly.
func TestK8sPodFailureSuffix(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()
	k, ok := sb.impl.(*k8sImpl)
	if !ok {
		t.Fatalf("expected a kubernetes sandbox, got %T", sb.impl)
	}
	podName := k.currentPodName()

	// A healthy pod must add nothing: this runs on every exec error, and a
	// wrong or noisy clause is worse than none.
	if got := k.podFailureSuffix(); got != "" {
		t.Errorf("a Running pod must contribute no suffix, got %q", got)
	}

	t.Run("evicted", func(t *testing.T) {
		fake.mu.Lock()
		fake.pods[podName].Status.Phase = "Failed"
		fake.pods[podName].Status.Reason = "Evicted"
		fake.pods[podName].Status.Message = `Usage of EmptyDir volume "tmp" exceeds the limit "128Mi".`
		fake.mu.Unlock()
		got := k.podFailureSuffix()
		for _, want := range []string{"Evicted", "128Mi"} {
			if !strings.Contains(got, want) {
				t.Errorf("eviction suffix must name %q, got %q", want, got)
			}
		}
	})

	t.Run("oom killed", func(t *testing.T) {
		fake.mu.Lock()
		fake.pods[podName].Status.Phase = "Running"
		fake.pods[podName].Status.Reason = ""
		fake.pods[podName].Status.Message = ""
		cs := k8sContainerStatus{Name: sandboxContainerName}
		cs.State.Terminated = &k8sContainerTerminated{Reason: "OOMKilled", ExitCode: 137}
		fake.pods[podName].Status.ContainerStatuses = []k8sContainerStatus{cs}
		fake.mu.Unlock()
		got := k.podFailureSuffix()
		for _, want := range []string{"OOMKilled", "137"} {
			if !strings.Contains(got, want) {
				t.Errorf("OOM suffix must name %q, got %q", want, got)
			}
		}
	})

	t.Run("pod gone", func(t *testing.T) {
		fake.mu.Lock()
		delete(fake.pods, podName)
		fake.mu.Unlock()
		got := k.podFailureSuffix()
		if !strings.Contains(got, "gone") {
			t.Errorf("a vanished pod must say so, got %q", got)
		}
	})
}

// The suffix is a diagnostic, so it must never become a second failure — but
// silence is its own failure mode. An apiserver that cannot answer leaves the
// original error intact and says plainly that the cause could not be
// established, rather than leaving a gap for a model to fill with a guess.
func TestK8sPodFailureSuffixNamesAnUnreachableApiserver(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()
	k := sb.impl.(*k8sImpl)

	// Take the apiserver away entirely: any answer the suffix could give here
	// would be invented. (The fake's `denied` map only drives preflight's
	// access review, so it would leave this GET succeeding and the assertion
	// below passing for the wrong reason.)
	fake.srv.Close()

	got := k.podFailureSuffix()
	if !strings.Contains(got, "could not be asked") || !strings.Contains(got, "apiserver") {
		t.Errorf("an unreachable apiserver must say so, got %q", got)
	}
	// …and it must stay a short clause appended to the real error, never a
	// replacement for it.
	if len(got) > 120 {
		t.Errorf("the clause must stay short, got %d chars: %q", len(got), got)
	}
}
