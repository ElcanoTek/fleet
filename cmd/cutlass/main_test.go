package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ElcanoTek/fleet/internal/fakellm"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestLoadTaskYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.yaml")
	const body = `
prompt: "do the thing"
model: anthropic/claude-opus-4.8
fallback_model: anthropic/claude-sonnet-4-6
max_iterations: 7
mcp_selection:
  - server: sendgrid
    account: client_a
  - server: fast_io
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	task, err := loadTaskYAML(path)
	if err != nil {
		t.Fatalf("loadTaskYAML: %v", err)
	}
	if task.Prompt != "do the thing" {
		t.Errorf("prompt = %q", task.Prompt)
	}
	if task.Model == nil || *task.Model != "anthropic/claude-opus-4.8" {
		t.Errorf("model = %v", task.Model)
	}
	if task.FallbackModel == nil || *task.FallbackModel != "anthropic/claude-sonnet-4-6" {
		t.Errorf("fallback = %v", task.FallbackModel)
	}
	if task.MaxIterations == nil || *task.MaxIterations != 7 {
		t.Errorf("max_iterations = %v", task.MaxIterations)
	}
	if len(task.MCPSelection) != 2 || task.MCPSelection[0].Server != "sendgrid" ||
		task.MCPSelection[0].Account != "client_a" || task.MCPSelection[1].Server != "fast_io" {
		t.Errorf("mcp_selection = %+v", task.MCPSelection)
	}

	// A prompt-less task is rejected.
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("model: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTaskYAML(bad); err == nil {
		t.Error("expected error for prompt-less task")
	}
}

// repoConfigDir resolves the in-repo generic bundle (config/default) from this
// test file's location, so the harness has a bundle to load without depending on
// the working directory.
func repoConfigDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = <root>/cmd/cutlass/main_test.go
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	return filepath.Join(root, "config", "default")
}

// startFakeLLM wires the wire-compatible fake LLM and points the agent runtime at
// it via OPENROUTER_BASE_URL + a placeholder key. The scheduled finish enforcement
// (agentcore ScheduledPolicy) requires a confirm_audit call before the run may
// finish, so the scenario performs that self-audit, then replies with final text.
func startFakeLLM(t *testing.T, reply string) {
	t.Helper()
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{
		// validateConfirmAuditArgs requires non-empty reasoning + at least one
		// artifacts_checked / workflow_sections_checked / critical_actions entry,
		// and send_contract_checked present. "none" is not a critical-tool suffix,
		// so it satisfies the schema without committing an action that must execute.
		fakellm.ToolStep(fakellm.ToolCall{ID: "audit-1", Name: "confirm_audit", Arguments: `{"success":true,` +
			`"reasoning":"no-op task: nothing produced, nothing to verify",` +
			`"artifacts_checked":["n/a"],"workflow_sections_checked":["task"],` +
			`"critical_actions":[{"tool":"none"}],"send_contract_checked":false,` +
			`"attachments_checked":[],"remaining_risks":[]}`}),
		fakellm.TextStep(reply),
	}})
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	t.Setenv("FLEET_CLIENT_CONFIG_DIR", repoConfigDir(t))
	t.Setenv("FLEET_MOCK_MODE", "1") // host-mode sandbox: no podman / image needed
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("OPENROUTER_BASE_URL", ts.URL+"/api/v1")
}

func writeTaskFile(t *testing.T, prompt string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.yaml")
	body := "prompt: " + prompt + "\nmodel: anthropic/claude-opus-4.8\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCutlassOneShot_FakeLLM runs a full task to completion through the governed
// scheduled runtime with no provider (fake LLM), no DB, and no HTTP server —
// proving the issue's acceptance: a single task YAML runs locally and writes a
// parseable session log in an isolated workspace.
func TestCutlassOneShot_FakeLLM(t *testing.T) {
	startFakeLLM(t, "cutlass one-shot ok")
	taskFile := writeTaskFile(t, `"say hello"`)
	logPath := filepath.Join(t.TempDir(), "session.json")
	wsDir := filepath.Join(t.TempDir(), "ws")

	if err := run([]string{"--log", logPath, "--workspace", wsDir, taskFile}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The isolated workspace must exist.
	if fi, err := os.Stat(wsDir); err != nil || !fi.IsDir() {
		t.Fatalf("workspace dir not created: %v", err)
	}

	// The session log must exist and parse as a models.LogSession with content.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	var session models.LogSession
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatalf("session log is not valid JSON: %v\n%s", err, data)
	}
	if len(session.Messages) == 0 {
		t.Fatalf("session log has no messages: %s", data)
	}
}

// TestCutlassFreshWorkspacePerRun proves two runs without --workspace get
// distinct, isolated workspace dirs minted under the configured workspace root.
func TestCutlassFreshWorkspacePerRun(t *testing.T) {
	startFakeLLM(t, "ok")
	base := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", base)

	for i := 0; i < 2; i++ {
		taskFile := writeTaskFile(t, `"hello"`)
		logPath := filepath.Join(t.TempDir(), "session.json")
		if err := run([]string{"--log", logPath, taskFile}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) >= len("cutlass-run-") && e.Name()[:len("cutlass-run-")] == "cutlass-run-" {
			runs++
		}
	}
	if runs != 2 {
		t.Fatalf("expected 2 distinct per-run workspace dirs under %s, found %d", base, runs)
	}
}
