package sandbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// bridgeRequestCtx is a run_python request's context: its request, turn and
// conversation ids.
func bridgeRequestCtx() context.Context {
	return logging.WithRequestID(logging.WithTurn(context.Background(), "conv-9", "turn-3"), "req-77")
}

// captureBridgeLog makes the default logger write JSON into a buffer.
func captureBridgeLog(t *testing.T) *lockedSlogBuf {
	t.Helper()
	out := &lockedSlogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return out
}

// requireAttributedTraceback waits for the bridge stderr record holding
// marker and requires it to carry the request's ids (#2074 review: bridge
// stderr was logged with no request context at all).
func requireAttributedTraceback(t *testing.T, out *lockedSlogBuf, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, line := range strings.Split(out.String(), "\n") {
			if !strings.Contains(line, marker) {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("record %q: %v", line, err)
			}
			if m["subprocess"] != "sandbox-bridge" || m[logging.KeyRequestID] != "req-77" ||
				m[logging.KeyTurnID] != "turn-3" || m[logging.KeyConvID] != "conv-9" {
				t.Fatalf("bridge stderr not attributed to the request in flight: %s", line)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no bridge stderr record with %q:\n%s", marker, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// podman backend: a fake podman runs a bridge that prints a traceback on
// stderr, then answers.
func TestContainerBridgeStderrCarriesTheRequestContext(t *testing.T) {
	out := captureBridgeLog(t)
	bin := filepath.Join(t.TempDir(), "fake-podman")
	script := "#!/bin/sh\nwhile read line; do echo 'Traceback: boom-container' >&2; echo '{\"status\":\"ok\",\"result\":\"x\"}'; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &containerImpl{cfg: ContainerConfig{PodmanBinary: bin}}
	c.containerID = "chat-sandbox-stderr-ctx-test"
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.terminateBridgeLocked()
	}()
	if _, err := c.runPython(bridgeRequestCtx(), PythonRequest{Code: "1/0"}); err != nil {
		t.Fatalf("runPython: %v", err)
	}
	requireAttributedTraceback(t, out, "boom-container")
}

// kubernetes backend: the fake apiserver's bridge writes a traceback on the
// exec session's stderr channel before each response.
func TestK8sBridgeStderrCarriesTheRequestContext(t *testing.T) {
	out := captureBridgeLog(t)
	fake := newFakeKube(t)
	fake.bridgeStderr = []byte("Traceback: boom-k8s\n")
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()
	if _, err := sb.RunPython(bridgeRequestCtx(), PythonRequest{Code: "1/0"}); err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	requireAttributedTraceback(t, out, "boom-k8s")
}

// With no request in flight (past the grace window) a line carries the
// session-level ids only — never a finished request's.
func TestBridgeStderrFallsBackToTheSessionContext(t *testing.T) {
	l := bridgeStderrLogger(bridgeRequestCtx())
	out := captureBridgeLog(t)
	_, _ = l.Write([]byte("idle line\n"))
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &m); err != nil {
		t.Fatalf("%q: %v", out.String(), err)
	}
	if m[logging.KeyConvID] != "conv-9" || m[logging.KeyRequestID] != nil || m[logging.KeyTurnID] != nil {
		t.Fatalf("idle line = %v, want the session's conv_id only", m)
	}
}
