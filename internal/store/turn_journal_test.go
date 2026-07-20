package store

// #798 turn-journal store tests: per-stage crash recovery, projection
// idempotency, terminal-commit gating, and provenance isolation. Each test
// seeds the exact durable state a crash at that stage leaves behind — a crash
// after a durable write is indistinguishable from process death, so no real
// crash is needed.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func seedConvAndTurn(t *testing.T, s *Store, turnID string) string {
	t.Helper()
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "u@example.com", "t", "", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.CreateTurn(ctx, turnID, conv.ID, time.Now().Unix()); err != nil {
		t.Fatalf("CreateTurn: %v", err)
	}
	return conv.ID
}

func userEntry(t *testing.T, text string) agent.HistoryEntry {
	t.Helper()
	return agent.HistoryEntry{Role: "user", Type: "text", Content: mustJSON(t, agent.TextContent{Text: text})}
}

func loadEntries(t *testing.T, s *Store, convID string) []agent.HistoryEntry {
	t.Helper()
	entries, err := s.LoadHistory(context.Background(), convID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	return entries
}

func TestCommitUserMessage_DurableBeforeRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")

	id, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "hello"))
	if err != nil {
		t.Fatalf("CommitUserMessage: %v", err)
	}
	if id <= 0 {
		t.Fatalf("want a real messages.id, got %d", id)
	}
	got := loadEntries(t, s, convID)
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("user entry not durable: %+v", got)
	}
	// Provenance recorded.
	var seq int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT turn_seq FROM messages WHERE id = $1`, id).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("user entry turn_seq = %d, want 1", seq)
	}
}

func TestCommitTurnHistory_GatesAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	entries := []agent.HistoryEntry{
		{Role: "assistant", Type: "text", Content: mustJSON(t, agent.TextContent{Text: "answer"})},
	}
	if _, err := s.CommitTurnHistory(ctx, convID, "t1", entries); err != nil {
		t.Fatalf("CommitTurnHistory: %v", err)
	}
	// A retry after an ambiguous outcome reports the dedicated sentinel and
	// writes nothing.
	if _, err := s.CommitTurnHistory(ctx, convID, "t1", entries); err == nil || !strings.Contains(err.Error(), "already committed") {
		t.Fatalf("second commit: want ErrTurnHistoryCommitted, got %v", err)
	}
	if got := loadEntries(t, s, convID); len(got) != 2 {
		t.Fatalf("history rows = %d, want 2 (no duplicates)", len(got))
	}
	var committed int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT history_committed_at FROM turns WHERE turn_id = 't1'`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed == 0 {
		t.Fatal("history_committed_at not stamped")
	}
}

func TestBranchCopyKeepsProvenanceNull(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	ids, err := s.CommitTurnHistory(ctx, convID, "t1", []agent.HistoryEntry{
		{Role: "assistant", Type: "text", Content: mustJSON(t, agent.TextContent{Text: "a"})},
	})
	if err != nil || len(ids) != 1 {
		t.Fatalf("CommitTurnHistory: %v ids=%v", err, ids)
	}
	branch, err := s.BranchConversation(ctx, "u@example.com", convID, ids[0], "branch")
	if err != nil {
		t.Fatalf("BranchConversation: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE conversation_id = $1 AND turn_id IS NOT NULL`,
		branch.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("branch copy carried provenance on %d rows; must be NULL", n)
	}
}

// journalIntent / journalResult seed helpers.
func journalIntent(t *testing.T, s *Store, turnID string, seq int64, callID, name, input string) {
	t.Helper()
	if err := s.InsertTurnJournal(context.Background(), TurnJournalRow{
		TurnID: turnID, Seq: seq, Kind: TurnJournalIntent, CallID: callID,
		ToolName: name, Content: input, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("insert intent: %v", err)
	}
}

func journalResult(t *testing.T, s *Store, turnID string, seq int64, callID, text string) {
	t.Helper()
	if err := s.InsertTurnJournal(context.Background(), TurnJournalRow{
		TurnID: turnID, Seq: seq, Kind: TurnJournalResult, CallID: callID,
		ToolName: "bash", Content: text, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("insert result: %v", err)
	}
}

func seedEvent(t *testing.T, s *Store, turnID string, eventID uint64, name string, payload map[string]any) {
	t.Helper()
	data, _ := json.Marshal(payload)
	if err := s.InsertTurnEvents(context.Background(), []TurnEvent{{
		TurnID: turnID, EventID: eventID, Name: name, Data: data, CreatedAt: time.Now().Unix(),
	}}); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func recoveredHistory(t *testing.T, s *Store, convID string) []agent.HistoryEntry {
	t.Helper()
	recs, err := s.RecoverStrandedTurns(context.Background())
	if err != nil {
		t.Fatalf("RecoverStrandedTurns: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("recovered %d turns, want exactly 1", len(recs))
	}
	return loadEntries(t, s, convID)
}

// assertPaired asserts every tool_call is immediately followed by its
// tool_result — the provider-validity invariant replayHistory depends on.
func assertPaired(t *testing.T, entries []agent.HistoryEntry) {
	t.Helper()
	for i, e := range entries {
		if e.Type != "tool_call" {
			continue
		}
		var call agent.ToolCallContent
		if err := json.Unmarshal(e.Content, &call); err != nil {
			t.Fatalf("bad tool_call content: %v", err)
		}
		if i+1 >= len(entries) || entries[i+1].Type != "tool_result" {
			t.Fatalf("tool_call %s has no adjacent result", call.ID)
		}
		var res agent.ToolResultContent
		if err := json.Unmarshal(entries[i+1].Content, &res); err != nil {
			t.Fatalf("bad tool_result content: %v", err)
		}
		if res.ID != call.ID {
			t.Fatalf("result id %s does not pair call id %s", res.ID, call.ID)
		}
	}
}

func TestRecovery_CrashAfterUserCommitOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}

	got := recoveredHistory(t, s, convID)
	// Nothing to reconstruct: just the user row; turn flipped to error.
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("unexpected recovered history: %+v", got)
	}
	turn, err := s.LookupTurn(ctx, "t1")
	if err != nil || turn == nil {
		t.Fatalf("LookupTurn: %v %v", turn, err)
	}
	if turn.Status != TurnStatusError {
		t.Fatalf("turn status = %s, want error", turn.Status)
	}
}

func TestRecovery_CrashAfterIntentSynthesizesUnknownOutcome(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "send the email")); err != nil {
		t.Fatal(err)
	}
	journalIntent(t, s, "t1", 1, "call-9", "mcp_sendgrid_send", `{"to":"x@y.z"}`)
	// No result row, no tool.call SSE event (async write lost in the crash).

	got := recoveredHistory(t, s, convID)
	assertPaired(t, got)
	var sawWarning bool
	for _, e := range got {
		if e.Type == "tool_result" && strings.Contains(string(e.Content), "outcome is UNKNOWN") {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Fatalf("no unknown-outcome warning in recovered history: %+v", got)
	}
	// Reconciliation marker persisted and queryable.
	rows, err := s.LoadTurnJournal(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var marker bool
	for _, r := range rows {
		if r.Kind == TurnJournalResult && r.CallID == "call-9" && r.Synthesized {
			marker = true
		}
	}
	if !marker {
		t.Fatal("synthesized reconciliation marker missing from journal")
	}
}

func TestRecovery_JournaledResultAndTextProjected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	// Full governed result journaled; SSE carries interleaved text + the call.
	seedEvent(t, s, "t1", 1, "text.delta", map[string]any{"text": "Let me check. "})
	seedEvent(t, s, "t1", 2, "tool.call", map[string]any{"id": "call-1", "name": "bash", "input": `{"cmd":"ls"}`})
	journalIntent(t, s, "t1", 1, "call-1", "bash", `{"cmd":"ls"}`)
	full := strings.Repeat("full governed output line\n", 400) // ≫ 4 KB SSE preview
	journalResult(t, s, "t1", 2, "call-1", full)
	seedEvent(t, s, "t1", 3, "text.delta", map[string]any{"text": "Found it"})

	got := recoveredHistory(t, s, convID)
	assertPaired(t, got)
	var sawFull, sawInterrupted bool
	for _, e := range got {
		if e.Type == "tool_result" && strings.Contains(string(e.Content), "full governed output line") {
			var res agent.ToolResultContent
			if err := json.Unmarshal(e.Content, &res); err != nil {
				t.Fatal(err)
			}
			if res.Text == full {
				sawFull = true
			}
		}
		if e.Type == "text" && strings.Contains(string(e.Content), "interrupted by a server restart") {
			sawInterrupted = true
		}
	}
	if !sawFull {
		t.Fatal("journaled full-fidelity result not projected byte-identically")
	}
	if !sawInterrupted {
		t.Fatal("explicit interrupted-turn marker missing")
	}
}

func TestRecovery_IdempotentAcrossRepeatedRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	journalIntent(t, s, "t1", 1, "call-2", "bash", `{}`)
	journalResult(t, s, "t1", 2, "call-2", "ok")

	first := recoveredHistory(t, s, convID)

	// Second recovery: no running turns remain — a strict no-op.
	recs, err := s.RecoverStrandedTurns(ctx)
	if err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("second recovery touched %d turns, want 0", len(recs))
	}
	if again := loadEntries(t, s, convID); len(again) != len(first) {
		t.Fatalf("row count changed across recoveries: %d -> %d", len(first), len(again))
	}
}

func TestRecovery_TurnRetryDropsAbandonedText(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t2")
	if _, err := s.CommitUserMessage(ctx, convID, "t2", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	seedEvent(t, s, "t2", 1, "text.delta", map[string]any{"text": "abandoned attempt text"})
	seedEvent(t, s, "t2", 2, "turn.retry", map[string]any{"attempt": 2})
	seedEvent(t, s, "t2", 3, "text.delta", map[string]any{"text": "kept text"})

	got := recoveredHistory(t, s, convID)
	for _, e := range got {
		if strings.Contains(string(e.Content), "abandoned attempt text") {
			t.Fatalf("rolled-back attempt text leaked into recovered history: %s", e.Content)
		}
	}
	var kept bool
	for _, e := range got {
		if e.Type == "text" && strings.Contains(string(e.Content), "kept text") {
			kept = true
		}
	}
	if !kept {
		t.Fatal("post-retry text lost")
	}
}

func TestRecovery_CommittedButUnfinishedTurnCompletes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	// Crash AFTER CommitTurnHistory but BEFORE FinishTurn: history is whole,
	// only the turn ledger is stale.
	if _, err := s.CommitTurnHistory(ctx, convID, "t1", []agent.HistoryEntry{
		{Role: "assistant", Type: "text", Content: mustJSON(t, agent.TextContent{Text: "done"})},
	}); err != nil {
		t.Fatal(err)
	}
	recs, err := s.RecoverStrandedTurns(ctx)
	if err != nil {
		t.Fatalf("RecoverStrandedTurns: %v", err)
	}
	if len(recs) != 1 || recs[0].Projected != 0 {
		t.Fatalf("recs = %+v, want 1 rec with nothing projected", recs)
	}
	turn, err := s.LookupTurn(ctx, "t1")
	if err != nil || turn == nil {
		t.Fatal(err)
	}
	// Not a zombie 'running' row, and not an error either — the answer landed.
	if turn.Status != TurnStatusCompleted {
		t.Fatalf("status = %s, want completed", turn.Status)
	}
	if got := loadEntries(t, s, convID); len(got) != 2 {
		t.Fatalf("history rows = %d, want 2 (no duplicate projection)", len(got))
	}
}

func TestRecovery_SkipsProjectionWhenConversationMovedOn(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t0")
	if _, err := s.CommitUserMessage(ctx, convID, "t0", userEntry(t, "old question")); err != nil {
		t.Fatal(err)
	}
	journalIntent(t, s, "t0", 1, "call-1", "bash", `{}`)
	journalResult(t, s, "t0", 2, "call-1", "stale result")
	// Recovery failed on the first boot; the user then completed a NEWER turn.
	if err := s.CreateTurn(ctx, "t2", convID, time.Now().Unix()+10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitUserMessage(ctx, convID, "t2", userEntry(t, "new question")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitTurnHistory(ctx, convID, "t2", []agent.HistoryEntry{
		{Role: "assistant", Type: "text", Content: mustJSON(t, agent.TextContent{Text: "new answer"})},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(ctx, "t2", TurnStatusCompleted, time.Now().Unix(), false); err != nil {
		t.Fatal(err)
	}

	recs, err := s.RecoverStrandedTurns(ctx)
	if err != nil {
		t.Fatalf("RecoverStrandedTurns: %v", err)
	}
	if len(recs) != 1 || recs[0].Projected != 0 {
		t.Fatalf("stale turn must terminate WITHOUT projecting: %+v", recs)
	}
	// The stale content must not appear after the newer exchange.
	for _, e := range loadEntries(t, s, convID) {
		if strings.Contains(string(e.Content), "stale result") {
			t.Fatalf("stale turn content projected after newer traffic: %s", e.Content)
		}
	}
	turn, _ := s.LookupTurn(ctx, "t0")
	if turn == nil || turn.Status != TurnStatusError {
		t.Fatalf("stale turn not terminated: %+v", turn)
	}
}

func TestInsertTurnJournal_AmbiguousRetryAbsorbed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConvAndTurn(t, s, "t1")
	row := TurnJournalRow{TurnID: "t1", Seq: 1, Kind: TurnJournalIntent, CallID: "c1",
		ToolName: "bash", Content: `{}`, CreatedAt: time.Now().Unix()}
	if err := s.InsertTurnJournal(ctx, row); err != nil {
		t.Fatal(err)
	}
	// A verbatim retry after an ambiguous write outcome must be a no-op
	// success — not a unique-key error that latches the degraded flag.
	if err := s.InsertTurnJournal(ctx, row); err != nil {
		t.Fatalf("ambiguous-outcome retry must be absorbed: %v", err)
	}
	rows, err := s.LoadTurnJournal(ctx, "t1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %d err=%v, want exactly 1", len(rows), err)
	}
}

func TestRecovery_BlockedCallGetsNeverDispatchedNotUnknown(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	// Journal ACTIVE (call-1 dispatched and completed); call-2 has a tool.call
	// SSE event but NO intent row — the barrier proves it never dispatched.
	journalIntent(t, s, "t1", 3, "call-1", "bash", `{}`)
	journalResult(t, s, "t1", 4, "call-1", "ok")
	seedEvent(t, s, "t1", 1, "tool.call", map[string]any{"id": "call-1", "name": "bash", "input": `{}`})
	seedEvent(t, s, "t1", 2, "tool.call", map[string]any{"id": "call-2", "name": "send_email", "input": `{}`})

	got := recoveredHistory(t, s, convID)
	assertPaired(t, got)
	var sawNeverDispatched, sawUnknown bool
	for _, e := range got {
		if e.Type != "tool_result" {
			continue
		}
		if strings.Contains(string(e.Content), "did NOT execute") {
			sawNeverDispatched = true
		}
		if strings.Contains(string(e.Content), "outcome is UNKNOWN") {
			sawUnknown = true
		}
	}
	if !sawNeverDispatched {
		t.Fatal("blocked call not classified as never-dispatched")
	}
	if sawUnknown {
		t.Fatal("never-dispatched call misclassified as unknown outcome")
	}
}

// #826: an interrupted turn's steered user message must be projected into
// recovered history. Recovery stamps history_committed_at, which makes boot
// recovery complete the steer's queue row as "durably in canonical history" —
// without the projection that claim is false and the instruction silently
// vanishes. Exactly once: a resilience re-drive can re-emit the same steer
// event, and the turn-start user.message (no steered flag) is committed
// separately at turn_seq=1 and must not double.
func TestRecovery_ProjectsSteeredUserMessageExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "draft the email")); err != nil {
		t.Fatal(err)
	}

	// The stream as the crash left it: the turn-start user echo, some text,
	// the steer (re-emitted once by a resilience re-drive), more text.
	seedEvent(t, s, "t1", 1, "user.message", map[string]any{"text": "draft the email"})
	seedEvent(t, s, "t1", 2, "text.delta", map[string]any{"text": "drafting"})
	seedEvent(t, s, "t1", 3, "user.message", map[string]any{"text": "also CC legal", "steered": true, "input_id": "in-1"})
	seedEvent(t, s, "t1", 4, "user.message", map[string]any{"text": "also CC legal", "steered": true, "input_id": "in-1"})
	seedEvent(t, s, "t1", 5, "text.delta", map[string]any{"text": "will do"})

	got := recoveredHistory(t, s, convID)
	var steers, originals int
	var steerIdx, preTextIdx, postTextIdx int
	for i, e := range got {
		if e.Role == "user" && strings.Contains(string(e.Content), "also CC legal") {
			steers++
			steerIdx = i
		}
		if e.Role == "user" && strings.Contains(string(e.Content), "draft the email") {
			originals++
		}
		if e.Type == "text" && strings.Contains(string(e.Content), "drafting") {
			preTextIdx = i
		}
		if e.Type == "text" && strings.Contains(string(e.Content), "will do") {
			postTextIdx = i
		}
	}
	if steers != 1 {
		t.Fatalf("steered message projected %d times, want exactly 1: %+v", steers, got)
	}
	if originals != 1 {
		t.Fatalf("turn-start user message projected %d times, want exactly 1 (the turn_seq=1 commit)", originals)
	}
	if preTextIdx >= steerIdx || steerIdx >= postTextIdx {
		t.Fatalf("steer out of stream order: pre=%d steer=%d post=%d", preTextIdx, steerIdx, postTextIdx)
	}
}

// #826 end to end: after recovery projects the steered text, boot queue
// recovery's "completed = durably in history" resolution of the injected row
// is truthful — the row completes AND the text is in canonical history.
func TestRecovery_InjectedSteerRowCompletesWithTextPreserved(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "draft the email")); err != nil {
		t.Fatal(err)
	}
	row, _, err := s.EnqueueInput(ctx, InputQueueRow{
		ID: "iq-steer", ConversationID: convID, UserEmail: "u@example.com",
		ClientInputID: "cli-steer", Message: "also CC legal", Attachments: "[]", Mode: InputModeSteer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkInputInjected(ctx, row.ID, "t1"); err != nil || !ok {
		t.Fatalf("inject: ok=%v err=%v", ok, err)
	}
	seedEvent(t, s, "t1", 1, "user.message", map[string]any{"text": "also CC legal", "steered": true, "input_id": "cli-steer"})

	// Boot order: stranded turns first (projects + stamps
	// history_committed_at), then the queue resolves against that record.
	if _, err := s.RecoverStrandedTurns(ctx); err != nil {
		t.Fatal(err)
	}
	requeued, completed, cancelled, err := s.RecoverInputQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 || completed != 1 || cancelled != 0 {
		t.Fatalf("requeued=%d completed=%d cancelled=%d, want 0/1/0", requeued, completed, cancelled)
	}
	var found bool
	for _, e := range loadEntries(t, s, convID) {
		if e.Role == "user" && strings.Contains(string(e.Content), "also CC legal") {
			found = true
		}
	}
	if !found {
		t.Fatal("queue row completed as 'durably in history' but the steered text is not in history")
	}
}
