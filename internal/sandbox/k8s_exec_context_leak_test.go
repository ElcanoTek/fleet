// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// A stream that ends on its own must release its session context, not just the
// streams a caller explicitly closes (#1264).
//
// client-go's websocket executor runs a keepalive for as long as the session
// context lives. When a sandbox pod is evicted or deleted mid-exec the stream
// dies by itself, and if nobody calls close() the ping loop kept writing to a
// dead socket — on the validation cluster one abandoned session logged 3,285
// "Websocket Ping failed" lines over fifteen hours, leaking a goroutine and a
// socket and burying every other line in the log.
func TestExecSessionReleasesItsContextWhenTheStreamEndsItself(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()
	k := sb.impl.(*k8sImpl)

	var out, errBuf bytes.Buffer
	sess, err := k.backend.client.execPod(context.Background(), "fleet-sandboxes",
		k.currentPodName(), sandboxContainerName, []string{"echo", "done"}, false, &out, &errBuf)
	if err != nil {
		t.Fatalf("execPod: %v", err)
	}

	// The command exits, so the stream ends on its own. Deliberately do NOT
	// call sess.close() — that is the abandoned-session case.
	select {
	case <-sess.done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream never ended")
	}

	// Give the deferred cancel a moment to run after done is closed.
	deadline := time.Now().Add(2 * time.Second)
	for sess.ctx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sess.ctx.Err() == nil {
		t.Error("session context still live after the stream ended — client-go's keepalive would ping forever")
	}
}
