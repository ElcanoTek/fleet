package store

import (
	"context"
	"strings"
	"testing"
)

// TestRecordPanic_RoundTrip verifies the recovered-panic ledger persists (#241).
func TestRecordPanic_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if n, err := s.CountPanics(ctx); err != nil || n != 0 {
		t.Fatalf("fresh store: count=%d err=%v, want 0", n, err)
	}
	const fakeSecret = "Authorization: Bearer fake-store-regression-secret"
	if err := s.RecordPanic(ctx, "runner.worker", fakeSecret, "stack "+fakeSecret); err != nil {
		t.Fatalf("RecordPanic: %v", err)
	}
	if err := s.RecordPanic(ctx, "httpapi.handler POST /chat", "index out of range", "stack2"); err != nil {
		t.Fatalf("RecordPanic 2: %v", err)
	}
	n, err := s.CountPanics(ctx)
	if err != nil {
		t.Fatalf("CountPanics: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	var legacyClass, legacyStack string
	if err := s.db.QueryRowContext(ctx,
		`SELECT message, stack FROM panic_events WHERE location = $1`, "runner.worker",
	).Scan(&legacyClass, &legacyStack); err != nil {
		t.Fatalf("read legacy panic: %v", err)
	}
	if legacyClass != "legacy" || legacyStack != "" || strings.Contains(legacyClass+legacyStack, fakeSecret) {
		t.Fatalf("legacy panic persisted raw diagnostics: class=%q stack=%q", legacyClass, legacyStack)
	}

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
	err = s.db.QueryRowContext(ctx, `
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
