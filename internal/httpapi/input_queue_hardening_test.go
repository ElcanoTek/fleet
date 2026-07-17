package httpapi

// Regression tests for the #785 queue hardening pass: the pending depth cap,
// the strict Stop-epoch comparison, rune-safe previews, and injected-row
// settlement on the ambiguous-commit replay branch.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// The depth cap bounds unattended LLM spend: once maxPendingInputs rows are
// queued, a fresh submission is refused (429) while an idempotent replay of an
// already-accepted input still answers 200.
func TestQueue_DepthCapRejectsOverflowButAllowsReplay(t *testing.T) {
	s := serverFixture(t)
	const user = "cap@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &gatedEngine{started: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	s.agent = eng

	go postChatJSON(t, s, user, map[string]any{"message": "long turn", "conversation_id": conv.ID})
	<-eng.started

	for i := 0; i < maxPendingInputs; i++ {
		w := postChatJSON(t, s, user, map[string]any{
			"message": fmt.Sprintf("queued %d", i), "conversation_id": conv.ID,
			"input_id": fmt.Sprintf("cap-%d", i),
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("submission %d: status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	// One over the cap: refused, and no row is created.
	w := postChatJSON(t, s, user, map[string]any{
		"message": "one too many", "conversation_id": conv.ID, "input_id": "cap-overflow",
	})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap submission: status=%d, want 429: %s", w.Code, w.Body.String())
	}
	// Idempotent replay of an accepted input still succeeds at the cap.
	w = postChatJSON(t, s, user, map[string]any{
		"message": "queued 0", "conversation_id": conv.ID, "input_id": "cap-0",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("replay at cap: status=%d, want 200: %s", w.Code, w.Body.String())
	}
	if n, err := s.store.CountPendingInputs(context.Background(), conv.ID); err != nil || n != maxPendingInputs {
		t.Fatalf("pending rows = %d (err=%v), want %d", n, err, maxPendingInputs)
	}

	eng.release <- struct{}{}
}

// created_at has second granularity: a row accepted in the same second as the
// Stop is a fresh post-Stop submission and must NOT be gated (strict <, not
// <=). A row from an earlier second stays gated.
func TestQueue_StopEpochGateIsStrict(t *testing.T) {
	s := &Server{}
	s.markStopAll("conv-1")
	s.inflightMu.Lock()
	epoch := s.stopEpochs["conv-1"]
	s.inflightMu.Unlock()

	if s.stoppedSince("conv-1", epoch) {
		t.Fatal("row accepted in the Stop's own second was gated — a 202-acknowledged post-Stop input would be silently cancelled")
	}
	if !s.stoppedSince("conv-1", epoch-1) {
		t.Fatal("row accepted before the Stop was not gated")
	}
	if s.stoppedSince("conv-2", epoch-1) {
		t.Fatal("un-stopped conversation was gated")
	}
}

// markStopAll prunes epochs older than the max turn lifetime so the map does
// not grow for the process lifetime.
func TestQueue_StopEpochsPruned(t *testing.T) {
	s := &Server{stopEpochs: map[string]int64{
		"ancient": time.Now().Unix() - int64(defaultTurnExecutionTimeout/time.Second) - 3600,
	}}
	s.markStopAll("fresh")
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, ok := s.stopEpochs["ancient"]; ok {
		t.Fatal("stale stop epoch survived pruning")
	}
	if _, ok := s.stopEpochs["fresh"]; !ok {
		t.Fatal("fresh stop epoch missing")
	}
}

// message_preview must clamp on a rune boundary — a raw byte slice of a
// multi-byte message emits invalid UTF-8 into every queue snapshot.
func TestQueueItemsPayloadRuneSafePreview(t *testing.T) {
	items := []store.InputQueueRow{{ID: "i1", Message: strings.Repeat("中", 60)}} // 180 bytes; byte 160 splits a rune
	payload := queueItemsPayload(items)
	preview, _ := payload[0]["message_preview"].(string)
	if !utf8.ValidString(preview) {
		t.Fatalf("preview is not valid UTF-8: %q", preview)
	}
	if !strings.HasSuffix(preview, "…") {
		t.Fatalf("long message was not truncated: %q", preview)
	}
}

// An ambiguous-commit retry (ErrTurnHistoryCommitted) must still settle the
// turn's injected steer rows: SettleTurnInputs skips them (history IS
// committed), so without the replay branch completing them they stay
// 'injected' in every queue snapshot until the next boot recovery.
func TestQueue_ReplayCommitCompletesInjectedRows(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	const user = "replay@x.com"
	conv, err := s.store.CreateConversation(ctx, user, "q", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	const turnID = "turn-replay"
	if err := s.store.CreateTurn(ctx, turnID, conv.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	row, _, err := s.store.EnqueueInput(ctx, store.InputQueueRow{
		ID: "steer-1", ConversationID: conv.ID, UserEmail: user,
		ClientInputID: "cli-steer-1", Message: "steer me", Attachments: "[]", Mode: store.InputModeSteer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.store.MarkInputInjected(ctx, row.ID, turnID); err != nil || !ok {
		t.Fatalf("MarkInputInjected: ok=%v err=%v", ok, err)
	}

	entries := []agent.HistoryEntry{{Role: "assistant", Type: "text", Content: []byte(`{"text":"a"}`)}}
	// Project the history directly — the "ambiguous outcome" where the commit
	// landed but the driver never saw the success (crash/timeout between the
	// DB commit and the response).
	if _, err := s.store.CommitTurnHistory(ctx, conv.ID, turnID, entries); err != nil {
		t.Fatalf("direct CommitTurnHistory: %v", err)
	}
	// The driver's retry now hits ErrTurnHistoryCommitted — the replay branch.
	commits := &turnCommits{store: s.store, convID: conv.ID, turnID: turnID,
		journal: newTurnJournalWriter(s.store, turnID)}
	if err := commits.commitTerminal(entries, false); err != nil {
		t.Fatalf("replay commitTerminal: %v", err)
	}
	items, err := s.store.ListQueuedInputs(ctx, user, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == row.ID {
			t.Fatalf("injected row still visible after replay commit (state=%s)", it.State)
		}
	}
}
