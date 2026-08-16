package store

import (
	"context"
	"testing"
)

// TestRecordPanicEvent_RoundTrip verifies the recovered-panic ledger persists
// attributed events (#795) and never stores a raw stack — only opaque
// attribution and the bounded class cross the persistence boundary.
func TestRecordPanicEvent_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	attributed := PanicEventRecord{
		IncidentID:     "inc_0123456789abcdef0123456789abcdef",
		Location:       "agentcore.tool",
		Boundary:       "tool.execute",
		ToolName:       "mcp_crm_create",
		ToolCallID:     "call-opaque",
		RunMode:        "scheduled",
		TaskID:         "task-opaque",
		ConversationID: "conversation-opaque",
		Class:          "string",
	}
	if err := s.RecordPanicEvent(ctx, attributed); err != nil {
		t.Fatalf("RecordPanicEvent: %v", err)
	}
	var got PanicEventRecord
	var storedStack string
	err := s.db.QueryRowContext(ctx, `
		SELECT incident_id, location, boundary, tool_name, tool_call_id,
		       run_mode, task_id, conversation_id, message, stack
		FROM panic_events WHERE incident_id = $1`, attributed.IncidentID).Scan(
		&got.IncidentID, &got.Location, &got.Boundary, &got.ToolName,
		&got.ToolCallID, &got.RunMode, &got.TaskID, &got.ConversationID,
		&got.Class, &storedStack,
	)
	if err != nil {
		t.Fatalf("read attributed panic: %v", err)
	}
	if got != attributed {
		t.Fatalf("panic event = %+v, want %+v", got, attributed)
	}
	if storedStack != "" {
		t.Fatalf("attributed panic stored stack %q, want empty", storedStack)
	}
}
