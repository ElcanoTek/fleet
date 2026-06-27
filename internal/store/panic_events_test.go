package store

import (
	"context"
	"testing"
)

// TestRecordPanic_RoundTrip verifies the recovered-panic ledger persists (#241).
func TestRecordPanic_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if n, err := s.CountPanics(ctx); err != nil || n != 0 {
		t.Fatalf("fresh store: count=%d err=%v, want 0", n, err)
	}
	if err := s.RecordPanic(ctx, "runner.worker", "nil map write", "goroutine 1 [running]:\n..."); err != nil {
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
}
