package taskrun

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/fakellm"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Sub-agents default-on e2e (#1043), through the full governed scheduled
// runtime with the fake-LLM seam — no real key, no network, no podman
// (FLEET_MOCK_MODE host sandbox). Proves the acceptance matrix end to end:
// a DEFAULT task (no allow_delegation key) advertises spawn_subagent and can
// fan out ≥2 typed children; child spend is charged back to the parent; the
// parent log carries the subagent_spawned linkage entries and each child gets
// an isolated workdir + its own sibling log file; a sequential parent with the
// tool advertised finishes with zero spawns; and either kill switch
// (allow_delegation:false, FLEET_SUBAGENTS_ENABLED=false) structurally hides
// the tool.

// auditStep is the confirm_audit self-audit the scheduled finish enforcement
// requires before a run may end (same shape as startFakeLLM's).
func auditStep(id string) fakellm.Step {
	return fakellm.ToolStep(fakellm.ToolCall{ID: id, Name: "confirm_audit", Arguments: `{"success":true,` +
		`"reasoning":"no-op: nothing external produced",` +
		`"artifacts_checked":["n/a"],"workflow_sections_checked":["task"],` +
		`"critical_actions":[{"tool":"none"}],"send_contract_checked":false,` +
		`"attachments_checked":[],"remaining_risks":[]}`})
}

// startSubagentFakeLLM boots a fake-LLM server the test can register scenarios
// on, and points the runtime at it (mirrors startFakeLLM, which discards the
// server handle this test needs for Hits/SawTool assertions).
func startSubagentFakeLLM(t *testing.T) *fakellm.Server {
	t.Helper()
	fake := fakellm.New()
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	t.Setenv("FLEET_CLIENT_CONFIG_DIR", repoConfigDir(t))
	t.Setenv("FLEET_MOCK_MODE", "1")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("OPENROUTER_BASE_URL", ts.URL+"/api/v1")
	// Children derive their sibling log files from this base; without it they
	// would land in the package dir.
	t.Setenv("FLEET_LOG_FILE", filepath.Join(t.TempDir(), "fleet-session.json"))
	return fake
}

func writeTaskFileLines(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.yaml")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runTaskToLog(t *testing.T, taskFile string) models.LogSession {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "session.json")
	wsDir := filepath.Join(t.TempDir(), "ws")
	if err := run([]string{"--log", logPath, "--workspace", wsDir, taskFile}, "taskrun-test"); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	var session models.LogSession
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatalf("session log is not valid JSON: %v\n%s", err, data)
	}
	return session
}

// subagentSpawnedPayload mirrors recordSubagentSpawn's linkage entry.
type subagentSpawnedPayload struct {
	ChildSessionID string  `json:"child_session_id"`
	Role           string  `json:"role"`
	Workdir        string  `json:"workdir"`
	CostUSD        float64 `json:"cost_usd"`
	Tokens         int     `json:"tokens"`
	Success        bool    `json:"success"`
}

func spawnedPayloads(t *testing.T, session models.LogSession) []subagentSpawnedPayload {
	t.Helper()
	var out []subagentSpawnedPayload
	for _, m := range session.Messages {
		if m.MessageType == nil || *m.MessageType != "subagent_spawned" {
			continue
		}
		var p subagentSpawnedPayload
		if err := json.Unmarshal([]byte(m.Content), &p); err != nil {
			t.Fatalf("subagent_spawned payload is not JSON: %v\n%s", err, m.Content)
		}
		out = append(out, p)
	}
	return out
}

func TestSubagents_DefaultTaskFansOut_FakeLLM(t *testing.T) {
	fake := startSubagentFakeLLM(t)
	fake.Scenario("fanout-parent", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.ToolStep(
			fakellm.ToolCall{ID: "sp-1", Name: "spawn_subagent",
				Arguments: `{"task":"[[scenario:fanout-child-a]] summarize input A","role":"explore"}`},
			fakellm.ToolCall{ID: "sp-2", Name: "spawn_subagent",
				Arguments: `{"task":"[[scenario:fanout-child-b]] produce file B","role":"worker"}`},
		),
		auditStep("audit-p"),
		fakellm.TextStep("PARENT-DONE"),
	}})
	fake.Scenario("fanout-child-a", fakellm.Scenario{Steps: []fakellm.Step{
		auditStep("audit-a"), fakellm.TextStep("CHILD-A-DONE"),
	}})
	fake.Scenario("fanout-child-b", fakellm.Scenario{Steps: []fakellm.Step{
		auditStep("audit-b"), fakellm.TextStep("CHILD-B-DONE"),
	}})

	// A DEFAULT task: no allow_delegation key anywhere — default-on is the point.
	taskFile := writeTaskFileLines(t,
		`prompt: "[[scenario:fanout-parent]] do the fan-out"`,
		`model: anthropic/claude-opus-4.8`)
	session := runTaskToLog(t, taskFile)

	// Both children ran (their scenarios were hit) and their answers came back.
	if fake.Hits("fanout-child-a") == 0 || fake.Hits("fanout-child-b") == 0 {
		t.Fatalf("children did not run: hits a=%d b=%d", fake.Hits("fanout-child-a"), fake.Hits("fanout-child-b"))
	}

	// The parent log carries one linkage entry per child, with role, isolated
	// workdir, and spend.
	spawns := spawnedPayloads(t, session)
	if len(spawns) != 2 {
		t.Fatalf("subagent_spawned entries = %d, want 2\n", len(spawns))
	}
	roles := map[string]subagentSpawnedPayload{}
	workdirs := map[string]bool{}
	childTokens := 0
	for _, p := range spawns {
		roles[p.Role] = p
		if p.ChildSessionID == "" || !strings.HasPrefix(p.ChildSessionID, "subagent-") {
			t.Errorf("bad child_session_id %q", p.ChildSessionID)
		}
		if !strings.Contains(p.Workdir, string(filepath.Separator)+"subagents"+string(filepath.Separator)) {
			t.Errorf("workdir %q not under .../subagents/", p.Workdir)
		}
		if workdirs[p.Workdir] {
			t.Errorf("children shared workdir %q", p.Workdir)
		}
		workdirs[p.Workdir] = true
		if !p.Success {
			t.Errorf("child %s did not succeed", p.ChildSessionID)
		}
		if p.Tokens <= 0 {
			t.Errorf("child %s reports no token spend — charge-back evidence missing", p.ChildSessionID)
		}
		childTokens += p.Tokens
		// Linkage to the child's own transcript: the sibling log file exists.
		base := os.Getenv("FLEET_LOG_FILE")
		sibling := strings.TrimSuffix(base, ".json") + "." + p.ChildSessionID + ".json"
		if _, err := os.Stat(sibling); err != nil {
			t.Errorf("child sibling log %q missing: %v", sibling, err)
		}
	}
	if _, ok := roles["explore"]; !ok {
		t.Error("no explore-role child recorded")
	}
	if _, ok := roles["worker"]; !ok {
		t.Error("no worker-role child recorded")
	}

	// Charge-back: the parent session's totals include the children's spend on
	// top of the parent's own turns.
	if total := session.PromptTokens + session.CompletionTokens; total < childTokens+15 {
		t.Errorf("parent totals (%d tokens) do not include child spend (%d) + own turns — charge-back broken", total, childTokens)
	}

	// The children's answers reached the parent, and the parent finished.
	joined := ""
	for _, m := range session.Messages {
		joined += m.Content + "\n"
	}
	for _, want := range []string{"CHILD-A-DONE", "CHILD-B-DONE", "PARENT-DONE"} {
		if !strings.Contains(joined, want) {
			t.Errorf("parent transcript missing %q", want)
		}
	}

	// Typed rosters, end to end: the explore child was never offered write_file;
	// the worker child was.
	if fake.SawTool("fanout-child-a", "write_file") {
		t.Error("explore child was offered write_file — role strip broken")
	}
	if !fake.SawTool("fanout-child-b", "write_file") {
		t.Error("worker child was not offered write_file — worker roster broken")
	}
	// Depth 1: no child was offered the spawn tool itself.
	if fake.SawTool("fanout-child-a", "spawn_subagent") || fake.SawTool("fanout-child-b", "spawn_subagent") {
		t.Error("a child was offered spawn_subagent — depth wall broken")
	}
}

// TestSubagents_SequentialParentNeverSpawns proves "do not force fan-out": a
// default task sees the tool advertised and simply finishes without using it.
func TestSubagents_SequentialParentNeverSpawns(t *testing.T) {
	fake := startSubagentFakeLLM(t)
	fake.Scenario("seq-parent", fakellm.Scenario{Steps: []fakellm.Step{
		auditStep("audit-s"), fakellm.TextStep("SEQ-DONE"),
	}})
	taskFile := writeTaskFileLines(t,
		`prompt: "[[scenario:seq-parent]] just answer"`,
		`model: anthropic/claude-opus-4.8`)
	session := runTaskToLog(t, taskFile)

	if !fake.SawTool("seq-parent", "spawn_subagent") {
		t.Error("default task must advertise spawn_subagent (default-on)")
	}
	if got := spawnedPayloads(t, session); len(got) != 0 {
		t.Errorf("sequential parent spawned %d children, want 0", len(got))
	}
}

// TestSubagents_KillSwitchesHideTool proves both opt-outs are structural: with
// either the per-task allow_delegation:false or the fleet-wide flag off, the
// tool is not in the advertised roster at all.
func TestSubagents_KillSwitchesHideTool(t *testing.T) {
	t.Run("per-task opt-out", func(t *testing.T) {
		fake := startSubagentFakeLLM(t)
		fake.Scenario("optout-parent", fakellm.Scenario{Steps: []fakellm.Step{
			auditStep("audit-o"), fakellm.TextStep("OPTOUT-DONE"),
		}})
		taskFile := writeTaskFileLines(t,
			`prompt: "[[scenario:optout-parent]] just answer"`,
			`model: anthropic/claude-opus-4.8`,
			`allow_delegation: false`)
		_ = runTaskToLog(t, taskFile)
		if fake.SawTool("optout-parent", "spawn_subagent") {
			t.Error("allow_delegation:false must hide spawn_subagent (structural non-registration)")
		}
	})

	t.Run("fleet-wide kill switch", func(t *testing.T) {
		fake := startSubagentFakeLLM(t)
		t.Setenv("FLEET_SUBAGENTS_ENABLED", "false")
		fake.Scenario("fleetoff-parent", fakellm.Scenario{Steps: []fakellm.Step{
			auditStep("audit-f"), fakellm.TextStep("FLEETOFF-DONE"),
		}})
		taskFile := writeTaskFileLines(t,
			`prompt: "[[scenario:fleetoff-parent]] just answer"`,
			`model: anthropic/claude-opus-4.8`)
		_ = runTaskToLog(t, taskFile)
		if fake.SawTool("fleetoff-parent", "spawn_subagent") {
			t.Error("FLEET_SUBAGENTS_ENABLED=false must hide spawn_subagent (structural non-registration)")
		}
	})
}
