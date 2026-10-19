package agentcore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// barrierBases returns n best-effort stdio servers running the barrier
// fixture against dir, named s0..s(n-1), plus the selection that binds them.
func barrierBases(t *testing.T, dir string, n, barrier int) (map[string]MCPServerBase, MCPSelection) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "barrier_server.py"))
	if err != nil {
		t.Fatal(err)
	}
	bases := map[string]MCPServerBase{}
	selection := make(MCPSelection, 0, n)
	for i := range n {
		name := "s" + strconv.Itoa(i)
		bases[name] = MCPServerBase{
			Command: python,
			Args:    []string{script},
			BaseEnv: map[string]string{"BARRIER_DIR": dir, "BARRIER_N": strconv.Itoa(barrier)},
		}
		selection = append(selection, MCPChoice{Server: name})
	}
	return bases, selection
}

// barrierRecord is what one fixture saw: whether the barrier filled, and how
// many servers were initializing at once when it arrived.
type barrierRecord struct {
	reached bool
	active  int
}

// barrierSeen reads every fixture's record.
func barrierSeen(t *testing.T, dir string) []barrierRecord {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen []barrierRecord
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "seen-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var rec barrierRecord
		var reached int
		if _, err := fmt.Sscanf(string(b), "%d %d", &reached, &rec.active); err != nil {
			t.Fatalf("barrier record %q: %v", b, err)
		}
		rec.reached = reached == 1
		seen = append(seen, rec)
	}
	return seen
}

// TestBindMCPSelectionReport_ConnectsConcurrently pins that an interactive
// bind starts its servers at the same time: each fixture's initialize waits
// until all four have started, which a one-after-another bind never reaches.
func TestBindMCPSelectionReport_ConnectsConcurrently(t *testing.T) {
	dir := t.TempDir()
	bases, selection := barrierBases(t, dir, 4, 4)
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	registered, failed, err := BindMCPSelectionReport(context.Background(), client, selection, bases, "")
	if err != nil || len(failed) != 0 {
		t.Fatalf("bind: err=%v failed=%v", err, failed)
	}
	if got := strings.Join(registered, ","); got != "s0,s1,s2,s3" {
		t.Fatalf("registered = %s, want selection order s0,s1,s2,s3", got)
	}
	seen := barrierSeen(t, dir)
	if len(seen) != 4 {
		t.Fatalf("got %d barrier records, want 4", len(seen))
	}
	for _, rec := range seen {
		if !rec.reached {
			t.Fatal("a server waited out the barrier: the four did not start at the same time")
		}
	}
}

// TestBindMCPSelectionReport_CapsConcurrentConnects pins mcpBindConcurrency:
// ten servers bind, but no more than eight are ever initializing at once.
func TestBindMCPSelectionReport_CapsConcurrentConnects(t *testing.T) {
	dir := t.TempDir()
	const limit = 8 // literal, so raising mcpBindConcurrency fails this test
	bases, selection := barrierBases(t, dir, 10, limit)
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	registered, failed, err := BindMCPSelectionReport(context.Background(), client, selection, bases, "")
	if err != nil || len(failed) != 0 || len(registered) != 10 {
		t.Fatalf("bind: registered=%v failed=%v err=%v", registered, failed, err)
	}
	for _, rec := range barrierSeen(t, dir) {
		if rec.active > limit {
			t.Fatalf("%d servers were initializing at once, cap is %d", rec.active, limit)
		}
	}
}

// TestBindMCPSelectionReport_CancelledCallerSpawnsNothing pins that a turn
// already ended by its caller starts no server, Required or not.
func TestBindMCPSelectionReport_CancelledCallerSpawnsNothing(t *testing.T) {
	dir := t.TempDir()
	bases, selection := barrierBases(t, dir, 3, 1)
	bases["s0"] = MCPServerBase{Command: bases["s0"].Command, Args: bases["s0"].Args, BaseEnv: bases["s0"].BaseEnv, Required: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	_, _, err := BindMCPSelectionReport(ctx, client, selection, bases, "")
	if err == nil || !strings.Contains(err.Error(), `"s0"`) {
		t.Fatalf("err = %v, want the Required server's cancellation", err)
	}
	if seen := barrierSeen(t, dir); len(seen) != 0 {
		t.Fatalf("%d servers started for a cancelled caller", len(seen))
	}
}

// TestBindMCPSelectionReport_RetryRunsBindSequentially pins that a run
// carrying a connect-retry allowance still binds one server at a time, so
// the allowance is charged per wait exactly as before.
func TestBindMCPSelectionReport_RetryRunsBindSequentially(t *testing.T) {
	dir := t.TempDir()
	bases, selection := barrierBases(t, dir, 3, 1)
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	registered, _, err := BindMCPSelectionReport(mcp.WithConnectRetry(context.Background()), client, selection, bases, "")
	if err != nil || len(registered) != 3 {
		t.Fatalf("bind: registered=%v err=%v", registered, err)
	}
	seen := barrierSeen(t, dir)
	if len(seen) != 3 {
		t.Fatalf("got %d barrier records, want 3", len(seen))
	}
	for _, rec := range seen {
		if rec.active != 1 {
			t.Fatalf("%d servers were initializing at once, want 1 (sequential)", rec.active)
		}
	}
}

// TestBindMCPSelectionReport_ReportsInSelectionOrder pins that results do not
// depend on which server finished first: registered and failed both follow
// the selection.
func TestBindMCPSelectionReport_ReportsInSelectionOrder(t *testing.T) {
	dir := t.TempDir()
	bases, _ := barrierBases(t, dir, 2, 1)
	bases["bad0"] = MCPServerBase{Command: badCommand}
	bases["bad1"] = MCPServerBase{Command: badCommand, Args: []string{"x"}}
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	registered, failed, err := BindMCPSelectionReport(context.Background(), client,
		MCPSelection{{Server: "bad1"}, {Server: "s1"}, {Server: "bad0"}, {Server: "s0"}}, bases, "")
	if err != nil {
		t.Fatalf("best-effort failures must not abort: %v", err)
	}
	if got := strings.Join(registered, ","); got != "s1,s0" {
		t.Errorf("registered = %s, want s1,s0", got)
	}
	failedNames := make([]string, 0, len(failed))
	for _, f := range failed {
		failedNames = append(failedNames, f.Server)
	}
	if got := strings.Join(failedNames, ","); got != "bad1,bad0" {
		t.Errorf("failed = %s, want bad1,bad0", got)
	}
}

// TestBindMCPSelectionReport_RequiredFailureNamesCause pins that a Required
// failure aborts the bind with that server's error, even while best-effort
// servers bind alongside it.
func TestBindMCPSelectionReport_RequiredFailureNamesCause(t *testing.T) {
	dir := t.TempDir()
	bases, _ := barrierBases(t, dir, 1, 1)
	bases["loadbearing"] = MCPServerBase{Command: badCommand, Required: true}
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	_, _, err := BindMCPSelectionReport(context.Background(), client,
		MCPSelection{{Server: "s0"}, {Server: "loadbearing"}}, bases, "")
	if err == nil || !strings.Contains(err.Error(), `"loadbearing"`) {
		t.Fatalf("err = %v, want the Required server's registration error", err)
	}
}

// TestBindMCPSelectionReport_ConfigErrorSpawnsNothing pins that the whole
// selection is resolved before any server starts: an unknown server late in
// the selection leaves the client empty.
func TestBindMCPSelectionReport_ConfigErrorSpawnsNothing(t *testing.T) {
	dir := t.TempDir()
	bases, _ := barrierBases(t, dir, 1, 1)
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	if _, _, err := BindMCPSelectionReport(context.Background(), client,
		MCPSelection{{Server: "s0"}, {Server: "ghost"}}, bases, ""); err == nil {
		t.Fatal("an unknown server must abort the bind")
	}
	if tools := client.GetAllTools(); len(tools) != 0 {
		t.Fatalf("nothing may be spawned on a config error, got %d tools", len(tools))
	}
	if seen := barrierSeen(t, dir); len(seen) != 0 {
		t.Fatalf("a server started despite the config error: %v", seen)
	}
}

// TestBindMCPSelectionReport_RegistrationNameCollision pins that a repeated
// choice binds once, while two different choices that register under one
// name are refused before anything spawns.
func TestBindMCPSelectionReport_RegistrationNameCollision(t *testing.T) {
	dir := t.TempDir()
	bases, _ := barrierBases(t, dir, 1, 1)
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()

	registered, _, err := BindMCPSelectionReport(context.Background(), client,
		MCPSelection{{Server: "s0"}, {Server: "s0"}}, bases, "")
	if err != nil || strings.Join(registered, ",") != "s0" {
		t.Fatalf("repeated choice: registered=%v err=%v, want s0 once", registered, err)
	}

	t.Setenv("FOO_KEY_BAR", "variant")
	collide := map[string]MCPServerBase{
		"foo":     {Command: badCommand, BaseEnv: map[string]string{"FOO_KEY": "default"}},
		"foo_bar": {Command: badCommand},
	}
	_, _, err = BindMCPSelectionReport(context.Background(), mcp.NewClient(),
		MCPSelection{{Server: "foo", Account: "bar"}, {Server: "foo_bar"}}, collide, "")
	if err == nil || !strings.Contains(err.Error(), `both register as "foo_bar"`) {
		t.Fatalf("err = %v, want a registration-name collision", err)
	}
}
