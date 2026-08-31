// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A sandbox that is never scheduled used to fail with "not ready before start
// timeout" and nothing else, while the scheduler had already written the
// reason onto the pod (#1264). On this backend that is the likeliest way a
// start fails — a nodeSelector or toleration matching no node, or a node-pinned
// volume the sandbox cannot reach — and it is exactly the case with NO
// container status for the existing branch to report.
func TestWaitForRunningReportsTheSchedulingBlocker(t *testing.T) {
	fake := newFakeKube(t)
	fake.mu.Lock()
	fake.unschedulable = true
	fake.mu.Unlock()
	backend := fake.backend(t, KubernetesConfig{
		Namespace:    "fleet-sandboxes",
		StartTimeout: 2 * time.Second, // the pod never schedules; don't wait 2 minutes for that
	})

	_, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err == nil {
		t.Fatal("a pod that never schedules must fail the start")
	}
	// The operator needs the scheduler's own words, not just "not ready".
	for _, want := range []string{"never scheduled", "Unschedulable", "0/3 nodes are available", "untolerated taint"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("start error must carry %q, got: %v", want, err)
		}
	}
}

// podSchedulingBlocker is the pure part, so the shapes that matter can be
// pinned without a clock.
func TestPodSchedulingBlocker(t *testing.T) {
	cond := func(status, reason, msg string) *k8sPod {
		return &k8sPod{Status: k8sPodStatus{Conditions: []k8sPodCondition{
			{Type: "PodScheduled", Status: status, Reason: reason, Message: msg},
		}}}
	}

	if got := podSchedulingBlocker(cond("True", "", "")); got != "" {
		t.Errorf("a scheduled pod must report no blocker, got %q", got)
	}
	if got := podSchedulingBlocker(&k8sPod{}); got != "" {
		t.Errorf("a pod with no conditions must report no blocker, got %q", got)
	}
	if got := podSchedulingBlocker(cond("False", "Unschedulable", "0/3 nodes are available")); !strings.Contains(got, "Unschedulable") || !strings.Contains(got, "0/3") {
		t.Errorf("reason and message must both survive, got %q", got)
	}
	// A condition with neither reason nor message still says something, rather
	// than reading as "scheduling is fine".
	if got := podSchedulingBlocker(cond("False", "", "")); got == "" {
		t.Error("PodScheduled=False with no text must still report a blocker")
	}
	// A large cluster enumerates every node; one error must not become a wall.
	long := podSchedulingBlocker(cond("False", "Unschedulable", strings.Repeat("x", 4000)))
	if len(long) > 600 {
		t.Errorf("the message must be capped, got %d chars", len(long))
	}
}
