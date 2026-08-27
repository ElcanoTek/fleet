// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package a2a

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskStateMappingIsExhaustive pins the #1279 acceptance criterion: every
// fleet task status maps to a real A2A state. A new fleet status added to the
// lifecycle without a row in TaskStateFor fails here instead of silently
// reporting TASK_STATE_UNSPECIFIED on the wire.
func TestTaskStateMappingIsExhaustive(t *testing.T) {
	if len(models.AllTaskStatuses) == 0 {
		t.Fatal("models.AllTaskStatuses is empty — the guard would be vacuous")
	}
	for _, status := range models.AllTaskStatuses {
		state, ok := TaskStateFor(status)
		if !ok {
			t.Errorf("fleet status %q has no A2A mapping — add it to TaskStateFor", status)
			continue
		}
		if state == wire.TaskStateUnspecified {
			t.Errorf("fleet status %q maps to TASK_STATE_UNSPECIFIED, which this package must never emit", status)
		}
		// Terminality must agree: an A2A client treats a terminal state as
		// immutable, so fleet may not report terminal for a status that can
		// still move (or vice versa).
		if state.Terminal() != status.IsTerminal() {
			t.Errorf("fleet status %q (terminal=%v) maps to %q (terminal=%v) — terminality must agree",
				status, status.IsTerminal(), state, state.Terminal())
		}
	}
}

// TestFleetStatusesForRoundTrips pins the reverse mapping the ListTasks status
// filter uses: filtering by the state a status reports as must include that
// status.
func TestFleetStatusesForRoundTrips(t *testing.T) {
	for _, status := range models.AllTaskStatuses {
		state, _ := TaskStateFor(status)
		statuses, known := FleetStatusesFor(state)
		if !known {
			t.Errorf("state %q (from %q) unknown to FleetStatusesFor", state, status)
			continue
		}
		found := false
		for _, s := range statuses {
			if s == string(status) {
				found = true
			}
		}
		if !found {
			t.Errorf("FleetStatusesFor(%q) = %v does not include %q", state, statuses, status)
		}
	}
	// The two A2A states fleet never reports are known-but-empty, and an
	// unknown string is refused — the handler turns that into InvalidParams.
	for _, state := range []wire.TaskState{wire.TaskStateRejected, wire.TaskStateAuthRequired} {
		statuses, known := FleetStatusesFor(state)
		if !known || len(statuses) != 0 {
			t.Errorf("FleetStatusesFor(%q) = (%v, %v), want known and empty", state, statuses, known)
		}
	}
	if _, known := FleetStatusesFor(wire.TaskState("bogus")); known {
		t.Error("FleetStatusesFor accepted an unknown state")
	}
}

func testTask(status models.TaskStatus) *models.Task {
	created := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	return &models.Task{
		ID:        uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Prompt:    "do the thing",
		Status:    status,
		CreatedAt: created,
	}
}

func TestBuildTaskInputRequiredCarriesTheQuestion(t *testing.T) {
	task := testTask(models.TaskStatusPausedAwaitingInput)
	task.PendingQuestion = "Which environment should I deploy to?"

	out := BuildTask(task, "", true)
	if out.Status.State != wire.TaskStateInputRequired {
		t.Fatalf("state = %q, want INPUT_REQUIRED", out.Status.State)
	}
	if out.ContextID != task.ID.String() || string(out.ID) != task.ID.String() {
		t.Fatalf("id/contextId = %q/%q, want both %q", out.ID, out.ContextID, task.ID)
	}
	if out.Status.Message == nil || out.Status.Message.Parts[0].Text() != task.PendingQuestion {
		t.Fatalf("status.message must carry the pending question, got %+v", out.Status.Message)
	}
	if out.Status.Message.Role != wire.MessageRoleAgent {
		t.Fatalf("status.message role = %q, want ROLE_AGENT", out.Status.Message.Role)
	}
}

func TestBuildTaskTerminalShapes(t *testing.T) {
	errText := "the site returned 500"
	failed := testTask(models.TaskStatusError)
	failed.ErrorMessage = &errText
	if out := BuildTask(failed, "", true); out.Status.Message == nil || out.Status.Message.Parts[0].Text() != errText {
		t.Errorf("FAILED status.message should carry the error text, got %+v", out.Status.Message)
	}

	stopped := "stopped by api-key:ci"
	cancelled := testTask(models.TaskStatusCancelled)
	cancelled.Result = &stopped
	if out := BuildTask(cancelled, "", true); out.Status.State != wire.TaskStateCanceled ||
		out.Status.Message == nil || out.Status.Message.Parts[0].Text() != stopped {
		t.Errorf("CANCELED should carry the stop attribution, got %+v", out.Status)
	}
}

func TestBuildArtifactsForASuccessfulTask(t *testing.T) {
	result := "All done. Report attached."
	task := testTask(models.TaskStatusSuccess)
	task.Result = &result
	task.OutputJSON = json.RawMessage(`{"total": 42}`)
	task.Artifacts = json.RawMessage(`[{"name":"q3 report.pdf","path":"reports/q3 report.pdf","description":"the report","size":1234}]`)

	arts := BuildArtifacts(task, "https://fleet.example.com")
	if len(arts) != 3 {
		t.Fatalf("got %d artifacts, want 3 (result, output, file): %+v", len(arts), arts)
	}
	if arts[0].ID != "result" || arts[0].Parts[0].Text() != result {
		t.Errorf("result artifact wrong: %+v", arts[0])
	}
	if arts[1].ID != "output" {
		t.Errorf("output artifact wrong id: %+v", arts[1])
	}
	if data, ok := arts[1].Parts[0].Data().(map[string]any); !ok || data["total"] != float64(42) {
		t.Errorf("output artifact should carry the decoded JSON, got %#v", arts[1].Parts[0].Data())
	}
	file := arts[2]
	if file.ID != "file:reports/q3 report.pdf" || file.Name != "q3 report.pdf" || file.Description != "the report" {
		t.Errorf("file artifact metadata wrong: %+v", file)
	}
	wantURL := "https://fleet.example.com/v1/tasks/" + task.ID.String() + "/workspace/reports/q3%20report.pdf"
	if got := string(file.Parts[0].URL()); got != wantURL {
		t.Errorf("file URL = %q, want %q (per-segment escaping)", got, wantURL)
	}
	if file.Parts[0].MediaType != "application/pdf" {
		t.Errorf("media type = %q, want application/pdf", file.Parts[0].MediaType)
	}

	// The stop-attribution reuse of Result must NOT become a "result" artifact.
	cancelled := testTask(models.TaskStatusCancelled)
	cancelled.Result = &result
	if arts := BuildArtifacts(cancelled, ""); len(arts) != 0 {
		t.Errorf("a cancelled task must not grow a result artifact, got %+v", arts)
	}
}

// TestBuildTaskWireShape pins the ProtoJSON spellings on the marshaled task —
// the enum prefix and the oneof part member — so an SDK bump that changed the
// wire format would fail here, not in an integrator's client.
func TestBuildTaskWireShape(t *testing.T) {
	result := "done"
	task := testTask(models.TaskStatusSuccess)
	task.Result = &result

	raw, err := json.Marshal(BuildTask(task, "", true))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"state":"TASK_STATE_COMPLETED"`, `"artifactId":"result"`, `"text":"done"`} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled task missing %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"kind"`) {
		t.Errorf("marshaled task contains a v0.3 'kind' discriminator:\n%s", s)
	}
}
