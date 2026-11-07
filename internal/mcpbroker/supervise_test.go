package mcpbroker

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMain re-execs this test binary as a broker child when MCPBROKER_TEST_CHILD=1
// so SpawnClient can be exercised against a REAL subprocess (not only net.Pipe).
// The child serves a fake broker over its stdio and exits on EOF. It must write
// ONLY protocol frames to stdout, so it runs and exits before m.Run() — no test
// framework output ever reaches the stream the parent decodes.
func TestMain(m *testing.M) {
	if os.Getenv("MCPBROKER_TEST_CHILD") == "trace" {
		serveTraceChild()
		os.Exit(0)
	}
	if os.Getenv("MCPBROKER_TEST_CHILD") == "1" {
		_ = ServeStdio(context.Background(), &fakeBroker{echoTagAsText: true})
		os.Exit(0)
	}
	if os.Getenv("MCPBROKER_TEST_CHILD") == "flush" {
		// An EOF-driven exit with shutdown work after Serve returns (the
		// broker's tracing flush): it takes a moment, then leaves a marker.
		_ = ServeStdio(context.Background(), &fakeBroker{echoTagAsText: true})
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(os.Getenv("MCPBROKER_TEST_MARKER"), []byte("flushed"), 0o600)
		os.Exit(0)
	}
	if os.Getenv("MCPBROKER_TEST_CHILD") == "hang" {
		// A hung broker: it ignores EOF and SIGTERM.
		signal.Ignore(syscall.SIGTERM)
		_ = ServeStdio(context.Background(), &fakeBroker{echoTagAsText: true})
		select {}
	}
	os.Exit(m.Run())
}

func brokerChildCmd() *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "MCPBROKER_TEST_CHILD=1")
	cmd.Stderr = os.Stderr // surface a child panic / race report
	return cmd
}

// TestSpawnClient_RealSubprocessRoundTrip proves the whole spawn -> stdio -> Client
// path works against an actual child process: a CallMCP and a Ping round-trip.
func TestSpawnClient_RealSubprocessRoundTrip(t *testing.T) {
	client, stop, err := SpawnClient(brokerChildCmd())
	if err != nil {
		t.Fatalf("SpawnClient: %v", err)
	}
	defer func() { _ = stop() }()

	text, isErr, err := client.CallMCP(context.Background(), "deal_sheet", "lookup", map[string]any{"tag": "via-subprocess"})
	if err != nil {
		t.Fatalf("CallMCP over a real subprocess: %v", err)
	}
	if isErr || text != "via-subprocess" {
		t.Fatalf("(text=%q, isErr=%v), want the tag echoed back through the child", text, isErr)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping over the subprocess: %v", err)
	}
}

// stop gives the child a bounded grace to exit on EOF before it signals, so
// shutdown work after Serve returns (the broker's span flush) completes.
func TestSpawnClient_StopWaitsForEOFExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "flushed")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "MCPBROKER_TEST_CHILD=flush", "MCPBROKER_TEST_MARKER="+marker)
	cmd.Stderr = os.Stderr
	client, stop, err := SpawnClient(cmd)
	if err != nil {
		t.Fatalf("SpawnClient: %v", err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	_ = stop()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the child was signalled before its EOF-driven shutdown finished: %v", err)
	}
}

// A hung child (ignores EOF and SIGTERM) cannot block shutdown: stop escalates
// to SIGKILL within the grace periods.
func TestSpawnClient_StopBoundsAHungChild(t *testing.T) {
	prev := eofExitGrace
	eofExitGrace = 200 * time.Millisecond
	t.Cleanup(func() { eofExitGrace = prev })
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "MCPBROKER_TEST_CHILD=hang")
	cmd.Stderr = os.Stderr
	client, stop, err := SpawnClient(cmd)
	if err != nil {
		t.Fatalf("SpawnClient: %v", err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	start := time.Now()
	_ = stop()
	if d := time.Since(start); d > eofExitGrace+teardownGrace+2*time.Second {
		t.Fatalf("stop took %s for a hung child", d)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() && cmd.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("hung child not reaped by SIGKILL: %v", cmd.ProcessState)
	}
}

// TestSpawnClient_StopTearsDownChild proves stop() ends the child and that calls
// after teardown fail fast (and that stop is idempotent).
func TestSpawnClient_StopTearsDownChild(t *testing.T) {
	client, stop, err := SpawnClient(brokerChildCmd())
	if err != nil {
		t.Fatalf("SpawnClient: %v", err)
	}
	if _, _, err := client.CallMCP(context.Background(), "s", "t", map[string]any{"tag": "x"}); err != nil {
		t.Fatalf("pre-stop call: %v", err)
	}

	_ = stop() // a clean EOF-driven exit may return nil; that's fine

	if _, _, err := client.CallMCP(context.Background(), "s", "t", nil); err == nil {
		t.Fatal("a call after stop should fail (connection closed), not hang")
	}
	_ = stop() // idempotent
}
