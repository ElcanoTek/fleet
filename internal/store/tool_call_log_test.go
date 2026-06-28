package store

import (
	"context"
	"testing"
	"time"
)

// TestToolCallLog exercises RecordToolCalls + ListToolCalls against real
// Postgres, mirroring the existing store test pattern (newTestStore skips when no
// test DB is configured).
func TestToolCallLog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "audit@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "audit@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	now := time.Now().Unix()
	dur := int64(1500)
	entries := []ToolCallEntry{
		{
			ConversationID: conv.ID,
			TurnID:         "turn-1",
			UserEmail:      "audit@example.com",
			ToolName:       "bash",
			ArgsSummary:    `{"command":"ls"}`,
			ResultSummary:  "file1\nfile2",
			IsError:        false,
			StartedAt:      now,
			DurationMS:     &dur,
		},
		{
			ConversationID: conv.ID,
			TurnID:         "turn-1",
			UserEmail:      "audit@example.com",
			ToolName:       "run_python",
			ArgsSummary:    `{"code":"1/0"}`,
			ResultSummary:  "ZeroDivisionError",
			IsError:        true,
			StartedAt:      now + 5,
			// DurationMS intentionally nil — exercises the nullable column.
		},
	}
	if err := s.RecordToolCalls(ctx, entries); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	// Empty batch is a no-op (no error).
	if err := s.RecordToolCalls(ctx, nil); err != nil {
		t.Fatalf("RecordToolCalls(nil): %v", err)
	}

	// List all — newest first (run_python at now+5 precedes bash at now).
	got, err := s.ListToolCalls(ctx, conv.ID, "", 0, 50)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[0].ToolName != "run_python" || got[1].ToolName != "bash" {
		t.Errorf("expected newest-first ordering, got %s then %s", got[0].ToolName, got[1].ToolName)
	}
	if !got[0].IsError {
		t.Errorf("run_python row should be flagged is_error")
	}
	if got[0].DurationMS != nil {
		t.Errorf("run_python duration should be nil, got %d", *got[0].DurationMS)
	}
	if got[1].DurationMS == nil || *got[1].DurationMS != dur {
		t.Errorf("bash duration mismatch: %v", got[1].DurationMS)
	}
	if got[1].ID == 0 {
		t.Errorf("expected a generated id")
	}

	// Tool filter.
	bashOnly, err := s.ListToolCalls(ctx, conv.ID, "bash", 0, 50)
	if err != nil {
		t.Fatalf("ListToolCalls(bash): %v", err)
	}
	if len(bashOnly) != 1 || bashOnly[0].ToolName != "bash" {
		t.Fatalf("tool filter failed: %+v", bashOnly)
	}

	// from filter — exclude the earlier bash row.
	recent, err := s.ListToolCalls(ctx, conv.ID, "", now+1, 50)
	if err != nil {
		t.Fatalf("ListToolCalls(from): %v", err)
	}
	if len(recent) != 1 || recent[0].ToolName != "run_python" {
		t.Fatalf("from filter failed: %+v", recent)
	}

	// limit.
	limited, err := s.ListToolCalls(ctx, conv.ID, "", 0, 1)
	if err != nil {
		t.Fatalf("ListToolCalls(limit): %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit failed: got %d", len(limited))
	}

	// Scope isolation: a different conversation returns nothing.
	other, err := s.ListToolCalls(ctx, "no-such-conv", "", 0, 50)
	if err != nil {
		t.Fatalf("ListToolCalls(other): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("expected no rows for unknown conversation, got %d", len(other))
	}
}

// TestToolCallLogCascade confirms deleting a conversation removes its audit rows
// (the FK ON DELETE CASCADE, matching messages / turn_metrics).
func TestToolCallLogCascade(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.CreateUser(ctx, "cascade@example.com", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	conv, err := s.CreateConversation(ctx, "cascade@example.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.RecordToolCalls(ctx, []ToolCallEntry{{
		ConversationID: conv.ID,
		TurnID:         "turn-1",
		UserEmail:      "cascade@example.com",
		ToolName:       "bash",
		StartedAt:      time.Now().Unix(),
	}}); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}
	if err := s.Delete(ctx, "cascade@example.com", conv.ID); err != nil {
		t.Fatalf("Delete conversation: %v", err)
	}
	got, err := s.ListToolCalls(ctx, conv.ID, "", 0, 50)
	if err != nil {
		t.Fatalf("ListToolCalls after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected audit rows cascade-deleted, got %d", len(got))
	}
}
