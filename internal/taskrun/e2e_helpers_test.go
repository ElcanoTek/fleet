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

// Shared helpers for the e2e tests that drive the full governed scheduled
// runtime through the fake-LLM seam — no real key, no network, no podman
// (FLEET_MOCK_MODE host sandbox).

// auditStep is the confirm_audit self-audit the scheduled finish enforcement
// requires before a run may end (same shape as startFakeLLM's).
func auditStep(id string) fakellm.Step {
	return fakellm.ToolStep(fakellm.ToolCall{ID: id, Name: "confirm_audit", Arguments: `{"success":true,` +
		`"reasoning":"no-op: nothing external produced",` +
		`"artifacts_checked":["n/a"],"workflow_sections_checked":["task"],` +
		`"critical_actions":[{"tool":"none"}],"send_contract_checked":false,` +
		`"attachments_checked":[],"remaining_risks":[]}`})
}

// startScenarioFakeLLM boots a fake-LLM server the test can register scenarios
// on, and points the runtime at it (mirrors startFakeLLM, which discards the
// server handle a test needs for Hits/SawTool assertions).
func startScenarioFakeLLM(t *testing.T) *fakellm.Server {
	t.Helper()
	fake := fakellm.New()
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	t.Setenv("FLEET_CLIENT_CONFIG_DIR", repoConfigDir(t))
	t.Setenv("FLEET_MOCK_MODE", "1")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("OPENROUTER_BASE_URL", ts.URL+"/api/v1")
	// Keep the session log out of the package dir.
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
