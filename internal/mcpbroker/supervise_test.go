package mcpbroker

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// TestMain re-execs this test binary as a broker child when MCPBROKER_TEST_CHILD=1
// so SpawnClient can be exercised against a REAL subprocess (not only net.Pipe).
// The child serves a fake broker over its stdio and exits on EOF. It must write
// ONLY protocol frames to stdout, so it runs and exits before m.Run() — no test
// framework output ever reaches the stream the parent decodes.
func TestMain(m *testing.M) {
	if os.Getenv("MCPBROKER_TEST_CHILD") == "1" {
		_ = ServeStdio(context.Background(), &fakeBroker{echoTagAsText: true})
		os.Exit(0)
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
