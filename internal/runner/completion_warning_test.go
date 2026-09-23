package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A run that finished after its verifier could not answer is a success whose
// terminal message leads with the warning flag (#1602); the final answer the
// flag is prefixed to is left as the agent wrote it.
func TestSuccessMessageFlagsVerifierOutage(t *testing.T) {
	flag := agentcore.MessageTypeCompletionUnverifiedVerifierError
	session := &models.LogSession{Messages: []models.LogMessage{
		{Role: "assistant", Content: "Published v874."},
		{Role: "user", Content: "[" + flag + "] WARNING: …", MessageType: &flag},
	}}
	if got := successMessage(session); got != "["+flag+"] Published v874." {
		t.Fatalf("successMessage = %q", got)
	}
	if got := finalAssistantText(session); got != "Published v874." {
		t.Fatalf("the final answer itself changed: %q", got)
	}
	structured := &models.LogSession{Messages: []models.LogMessage{
		{Role: "assistant", Content: `{"ok":true}`},
		{Role: "user", Content: "warning", MessageType: &flag},
	}}
	if got := successMessage(structured); !strings.HasPrefix(got, "["+flag+"] Task completed successfully") {
		t.Fatalf("structured run lost the flag: %q", got)
	}
	other := "system_enforcement"
	plain := &models.LogSession{Messages: []models.LogMessage{
		{Role: "user", Content: "nudge", MessageType: &other},
		{Role: "assistant", Content: "Published v874."},
	}}
	if got := successMessage(plain); got != "Published v874." {
		t.Fatalf("an unflagged run's message changed: %q", got)
	}
}

// The completion predicate's event reaches an operator tailing the run.
func TestTaskStreamBuffer_ForwardsCompletionPredicateFrame(t *testing.T) {
	buf := newTaskStreamBuffer()
	buf.Observe("fleet.completion_predicate", map[string]any{"tool": "mcp_pages_record_refresh_check", "clause": "any_succeeded"})
	buf.Finish()
	rw := newSSERecorder()
	if err := buf.Attach(context.Background(), 0, rw); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	for _, want := range []string{"event: completion_predicate", `"tool":"mcp_pages_record_refresh_check"`} {
		if body := rw.Body(); !strings.Contains(body, want) {
			t.Errorf("expected %q in stream:\n%s", want, body)
		}
	}
}
