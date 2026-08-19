package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Auxiliary model-call metering (#1118), driver side. The compaction
// summarizer's own model call counts against the run (RecordUsage + a ceiling
// pre-check that degrades to truncation); the end-of-run verifier and the
// phone-a-friend review are host-side extras that stay off-ceiling but land in
// the session log's labeled aux-usage ledger.

// TestSummarizeDroppedMiddle_MetersUsage proves the summarizer's Generate call
// is metered through CompactionSummarizeInput.RecordUsage — before #1118 the
// call carried no usage sink at all, so a compaction on an already-huge run
// spent invisibly.
func TestSummarizeDroppedMiddle_MetersUsage(t *testing.T) {
	model := &itMockModel{generateText: "condensed brief"} // Generate reports 10 in / 5 out
	var got fantasy.Usage
	calls := 0
	in := agentcore.CompactionSummarizeInput{
		Droppable: []fantasy.Message{fantasy.NewUserMessage("old turn 1"), fantasy.NewUserMessage("old turn 2")},
		RecordUsage: func(u fantasy.Usage, _ fantasy.ProviderMetadata) {
			calls++
			got = u
		},
	}
	summary := summarizeDroppedMiddle(context.Background(), TurnConfig{Model: model, MaxTokens: 4096}, in)
	if summary != "condensed brief" {
		t.Fatalf("summary = %q, want the model's text", summary)
	}
	if calls != 1 {
		t.Fatalf("RecordUsage called %d times, want 1", calls)
	}
	if got.InputTokens != 10 || got.OutputTokens != 5 {
		t.Fatalf("metered usage = %d/%d, want 10/5", got.InputTokens, got.OutputTokens)
	}
}

// TestSummarizeDroppedMiddle_OverCeilingDegradesToTruncation pins the ceiling
// pre-check: when the run's cost/token ceiling is already met, the summarizer
// must NOT buy another model call — it degrades to the deterministic
// placeholder (the truncation path), which still relieves the context
// pressure.
func TestSummarizeDroppedMiddle_OverCeilingDegradesToTruncation(t *testing.T) {
	model := &itMockModel{generateText: "must not run"}
	in := agentcore.CompactionSummarizeInput{
		Droppable: []fantasy.Message{fantasy.NewUserMessage("old turn 1"), fantasy.NewUserMessage("old turn 2")},
		RecordUsage: func(fantasy.Usage, fantasy.ProviderMetadata) {
			t.Fatal("nothing to meter when the summarizer degraded to truncation")
		},
		OverCeiling: func() bool { return true },
	}
	summary := summarizeDroppedMiddle(context.Background(), TurnConfig{Model: model, MaxTokens: 4096}, in)
	if !strings.Contains(summary, "messages compacted") {
		t.Fatalf("over-ceiling summarizer did not use the deterministic placeholder: %q", summary)
	}
	model.mu.Lock()
	calls := model.generateCount
	model.mu.Unlock()
	if calls != 0 {
		t.Fatalf("over-ceiling summarizer reached the provider %d times, want 0", calls)
	}
}

// TestRunEndOfRunVerifier_RecordsAuxUsage proves the verifier's host-side call
// lands in the session log's labeled aux-usage ledger — off the run's
// ceilings (its documented semantics), but no longer attributed to nothing.
func TestRunEndOfRunVerifier_RecordsAuxUsage(t *testing.T) {
	model := &itMockModel{generateText: `{"missing_actions": [], "reasoning": "complete"}`}
	a := &Agent{fallbackModel: model, logSession: NewLogSession()}

	missing, err := a.runEndOfRunVerifier(context.Background(), "send the report", nil)
	if err != nil {
		t.Fatalf("runEndOfRunVerifier: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("unexpected missing actions: %v", missing)
	}
	recs := a.logSession.SnapshotAuxUsage()
	if len(recs) != 1 {
		t.Fatalf("aux usage records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Label != agentcore.AuxUsageEndOfRunVerifier {
		t.Errorf("label = %q, want %q", rec.Label, agentcore.AuxUsageEndOfRunVerifier)
	}
	if rec.Model != "mock-model" || rec.PromptTokens != 10 || rec.CompletionTokens != 5 {
		t.Errorf("record = %+v, want model=mock-model prompt=10 completion=5", rec)
	}
	// The headline session totals stay pure run spend: the verifier is
	// off-ceiling overhead, visible only in the labeled ledger.
	if a.logSession.PromptTokens != 0 || a.logSession.Cost != 0 {
		t.Errorf("verifier spend leaked into the headline totals: prompt=%d cost=%f",
			a.logSession.PromptTokens, a.logSession.Cost)
	}
}

// TestWriteLogFile_PersistsAuxUsage pins the captain's-log FILE half of the
// #1118 ledger: writeLogFile rebuilds the session through redactLogSession
// (and, when oversized, truncateLogSession), and both clones must carry
// aux_usage — before this fix they rebuilt field-by-field and silently
// dropped it, so the persisted file contradicted the design doc. The chain
// under test is the real one: a verifier call populates the ledger, then the
// deferred writeLogFile persists it.
func TestWriteLogFile_PersistsAuxUsage(t *testing.T) {
	model := &itMockModel{generateText: `{"missing_actions": [], "reasoning": "complete"}`}
	a := &Agent{fallbackModel: model, logSession: NewLogSession()}
	if _, err := a.runEndOfRunVerifier(context.Background(), "send the report", nil); err != nil {
		t.Fatalf("runEndOfRunVerifier: %v", err)
	}

	logFile := filepath.Join(t.TempDir(), "session.json")
	writeLogFile(a.logSession, logFile)

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read written log file: %v", err)
	}
	var persisted LogSession
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("written log file is not a LogSession: %v", err)
	}
	if len(persisted.AuxUsage) != 1 {
		t.Fatalf("written log file aux_usage records = %d, want 1 (redactLogSession must carry the ledger)", len(persisted.AuxUsage))
	}
	rec := persisted.AuxUsage[0]
	if rec.Label != agentcore.AuxUsageEndOfRunVerifier || rec.Model != "mock-model" || rec.PromptTokens != 10 || rec.CompletionTokens != 5 {
		t.Errorf("persisted record = %+v, want label=%s model=mock-model prompt=10 completion=5",
			rec, agentcore.AuxUsageEndOfRunVerifier)
	}
}

// TestTruncateLogSession_PreservesAuxUsage covers the size-cap path of the same
// fix: the truncation clone drops transcript bulk, never the tiny accounting
// rows — a truncated (i.e. biggest) run is exactly where unmetering overhead
// would hurt most.
func TestTruncateLogSession_PreservesAuxUsage(t *testing.T) {
	session := NewLogSession()
	session.AddAuxUsage(agentcore.AuxUsageRecord{
		Label: agentcore.AuxUsagePhoneAFriend, Model: "mock-model", PromptTokens: 10, CompletionTokens: 5,
	})
	for i := 0; i < 40; i++ {
		session.AddMessage(roleUser, strings.Repeat("bulk transcript line ", 40), nil, nil)
	}

	full, err := marshalLogSession(session)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data := truncateLogSession(session, len(full)/2) // force real truncation
	var truncated LogSession
	if err := json.Unmarshal(data, &truncated); err != nil {
		t.Fatalf("truncated log is not a LogSession: %v", err)
	}
	if len(truncated.Messages) >= 40 {
		t.Fatalf("truncation did not drop messages (got %d) — test would not exercise the clone", len(truncated.Messages))
	}
	if len(truncated.AuxUsage) != 1 || truncated.AuxUsage[0].Label != agentcore.AuxUsagePhoneAFriend {
		t.Fatalf("aux_usage lost in truncation: %+v", truncated.AuxUsage)
	}
}

// TestRunPhoneAFriendReview_RecordsAuxUsage is the reviewer-side twin of the
// verifier test above.
func TestRunPhoneAFriendReview_RecordsAuxUsage(t *testing.T) {
	reviewer := &itMockModel{generateText: `{"needs_revision": false, "issues": [], "reasoning": "ship it"}`}
	a := &Agent{logSession: NewLogSession()}

	issues, err := a.runPhoneAFriendReview(context.Background(), reviewer, "write the summary", "the summary", nil)
	if err != nil {
		t.Fatalf("runPhoneAFriendReview: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	recs := a.logSession.SnapshotAuxUsage()
	if len(recs) != 1 {
		t.Fatalf("aux usage records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Label != agentcore.AuxUsagePhoneAFriend {
		t.Errorf("label = %q, want %q", rec.Label, agentcore.AuxUsagePhoneAFriend)
	}
	if rec.Model != "mock-model" || rec.PromptTokens != 10 || rec.CompletionTokens != 5 {
		t.Errorf("record = %+v, want model=mock-model prompt=10 completion=5", rec)
	}
}
