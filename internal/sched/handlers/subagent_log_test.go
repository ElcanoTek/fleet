package handlers

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// sessionReferencesSubagent is the linkage half of the child-transcript
// endpoint's authorization (#1043): a child id must appear in a
// subagent_spawned entry of the task's own persisted log, or the fetch is
// refused regardless of the caller's task permissions.
func TestSessionReferencesSubagent(t *testing.T) {
	spawned := "subagent_spawned"
	other := "some_other_type"
	childID := "subagent-12345678-1234-1234-1234-123456789abc"
	session := &models.LogSession{Messages: []models.LogMessage{
		{Role: "assistant", Content: "working"},
		{Role: "tool", MessageType: &other, Content: `{"child_session_id":"` + childID + `"}`},
		{Role: "tool", MessageType: &spawned, Content: `{"parent_task_id":"t","child_session_id":"` + childID + `","role":"explore","success":true}`},
	}}

	if !sessionReferencesSubagent(session, childID) {
		t.Fatal("linkage entry present — must match")
	}
	if sessionReferencesSubagent(session, "subagent-99999999-9999-9999-9999-999999999999") {
		t.Fatal("unknown child id must not match")
	}
	// The id appearing under a NON-subagent_spawned message type is not linkage:
	// only the structured entry recordSubagentSpawn writes counts.
	onlyOther := &models.LogSession{Messages: []models.LogMessage{
		{Role: "tool", MessageType: &other, Content: `{"child_session_id":"` + childID + `"}`},
	}}
	if sessionReferencesSubagent(onlyOther, childID) {
		t.Fatal("a non-linkage message type must not authorize")
	}
	if sessionReferencesSubagent(nil, childID) {
		t.Fatal("nil session must not authorize")
	}
}
