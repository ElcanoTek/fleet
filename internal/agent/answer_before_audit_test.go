package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// #1826: a scheduled run whose model gives its answer, is then asked for the
// self-audit, and closes the audit round with only a pointer back to that
// answer used to store the pointer alone. These tests pin that the answer
// given before the audit stays part of the run's answer, composed with the
// post-audit text the way a verifier repair's is.

const (
	answerTable  = "| seed | value |\n|---|---|\n| 1 | 42 |\n| 2 | 17 |"
	auditPointer = "The audit is confirmed. The table is in my previous message."
	passedAudit  = `{"success":true,"critical_actions":[],"reasoning":"checked every value","artifacts_checked":["values"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
)

// answerThenAuditModel scripts the #1826 shape: call 0 closes the first round
// with answer (no tool call, so the finish enforcement asks for the audit),
// call 1 calls confirm_audit with audit, and call 2 closes the run with
// closing. It records the prompt of every call.
func answerThenAuditModel(answer, audit, closing string, prompts *[]string) *itMockModel {
	calls := 0
	return &itMockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		raw, _ := json.Marshal(call.Prompt)
		*prompts = append(*prompts, string(raw))
		return func(yield func(fantasy.StreamPart) bool) {
			switch step {
			case 0:
				if answer != "" {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "answer", Delta: answer})
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			case 1:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: audit})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			default:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "closing", Delta: closing})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}
		}, nil
	}}
}

// The issue's reproduction: the answer, then an audit round that only points
// back to it. The run's persisted answer is the table followed by the audit's
// closing text, and the audit request told the model that is how it composes.
func TestScheduledAnswerBeforeAuditIsKept(t *testing.T) {
	var prompts []string
	a := newTestScheduledAgent(t, answerThenAuditModel(answerTable, passedAudit, auditPointer, &prompts))
	if err := a.Execute(context.Background(), "Compute the value of every seed and answer with a table."); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if want := answerTable + "\n\n" + auditPointer; lastAssistantContent(a.logSession) != want {
		t.Fatalf("persisted answer = %q, want the table followed by the audit's closing text %q", lastAssistantContent(a.logSession), want)
	}
	if len(prompts) != 3 {
		t.Fatalf("model calls = %d, want 3 (answer, audit, closing)", len(prompts))
	}
	if !strings.Contains(prompts[1], "Before finishing: ") || !strings.Contains(prompts[1], auditAnswerNote) {
		t.Errorf("the audit request must tell the model its answer stays and what follows is appended: %s", prompts[1])
	}
	if !enforcementLogged(a.logSession, auditAnswerNote) {
		t.Error("the session log's audit request does not carry the note the model was given")
	}
}

// The end-of-run verifier judges the answer the run persists, so it sees the
// answer given before the audit too: judging the pointer alone, it would
// re-demand the table on every check.
func TestScheduledVerifierJudgesTheAnswerBeforeAudit(t *testing.T) {
	var prompts []string
	verifier := &recordingVerifier{replies: []string{`{"missing_actions":[],"reasoning":"the table is reported"}`}}
	a := newTestScheduledAgent(t, answerThenAuditModel(answerTable, passedAudit, auditPointer, &prompts))
	a.verifierModel = verifier
	if err := a.Execute(context.Background(), "Compute the value of every seed and answer with a table."); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if len(verifier.prompts) != 1 {
		t.Fatalf("verifier calls = %d, want 1", len(verifier.prompts))
	}
	if !strings.Contains(verifier.prompts[0], "| 1 | 42 |") || !strings.Contains(verifier.prompts[0], "previous message") {
		t.Errorf("the verifier must judge the pre-audit answer and the audit's closing text: %s", verifier.prompts[0])
	}
}

// A model that audits on its own first, has that confirm_audit refused, and
// then answers without retrying is refused with "Audit not confirmed", not
// with the audit request. Its answer is kept the same way.
func TestScheduledAnswerAfterARefusedOwnAuditIsKept(t *testing.T) {
	const misnamed = `{"success":true,"critical_actions":[{"tool":"send_email"}],"reasoning":"checked every value","artifacts_checked":["values"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
	var prompts []string
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		raw, _ := json.Marshal(call.Prompt)
		prompts = append(prompts, string(raw))
		return func(yield func(fantasy.StreamPart) bool) {
			switch step {
			case 0, 2:
				input := misnamed
				if step == 2 {
					input = passedAudit
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			case 1:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "answer", Delta: answerTable})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			default:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "closing", Delta: auditPointer})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	if err := a.Execute(context.Background(), "Compute the value of every seed and answer with a table."); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if len(prompts) != 4 {
		t.Fatalf("model calls = %d, want 4 (refused audit, answer, audit, closing)", len(prompts))
	}
	if !strings.Contains(prompts[2], "Audit not confirmed.") || !strings.Contains(prompts[2], auditAnswerNote) {
		t.Fatalf("the unconfirmed-audit refusal must carry the note: %s", prompts[2])
	}
	if want := answerTable + "\n\n" + auditPointer; lastAssistantContent(a.logSession) != want {
		t.Fatalf("persisted answer = %q, want %q", lastAssistantContent(a.logSession), want)
	}
}

// A model that restates its whole answer after the audit, as the note asks it
// not to, still gets it stored once.
func TestScheduledAnswerBeforeAuditRestatedIsStoredOnce(t *testing.T) {
	var prompts []string
	restated := "Audit passed; the values are final.\n\n" + answerTable
	a := newTestScheduledAgent(t, answerThenAuditModel(answerTable, passedAudit, restated, &prompts))
	if err := a.Execute(context.Background(), "Compute the value of every seed and answer with a table."); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if got := lastAssistantContent(a.logSession); got != restated {
		t.Fatalf("persisted answer = %q, want the restated answer once %q", got, restated)
	}
}

// A round that ends with no text gave no answer: nothing is kept and the audit
// request carries no note, exactly as before.
func TestScheduledTextlessRoundBeforeAuditKeepsNothing(t *testing.T) {
	var prompts []string
	a := newTestScheduledAgent(t, answerThenAuditModel("", passedAudit, answerTable, &prompts))
	if err := a.Execute(context.Background(), "Compute the value of every seed and answer with a table."); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if got := lastAssistantContent(a.logSession); got != answerTable {
		t.Fatalf("persisted answer = %q, want the closing text alone %q", got, answerTable)
	}
	if len(prompts) < 2 || strings.Contains(prompts[1], auditAnswerNote) {
		t.Error("a textless round must not tell the model an answer was kept")
	}
}

// An aborted audit is the run's last word on whether that answer stands, so
// the answer given before it is dropped: the run persists the abort's own
// closing text, as before.
func TestScheduledAbortedAuditDropsTheAnswerBeforeIt(t *testing.T) {
	var prompts []string
	const aborted = `{"success":false,"reasoning":"two values could not be verified","artifacts_checked":["values"],"workflow_sections_checked":["completion"],"send_contract_checked":false,"attachments_checked":[],"remaining_risks":["unverified values"],"user_visible_summary":"Two values could not be verified."}`
	const closing = "Aborted: two of the values could not be verified."
	a := newTestScheduledAgent(t, answerThenAuditModel("All done.\n\n"+answerTable, aborted, closing, &prompts))
	err := a.Execute(context.Background(), "Compute the value of every seed and answer with a table.")
	if !errors.Is(err, agentcore.ErrAuditAborted) {
		t.Fatalf("aborted run returned %v, want ErrAuditAborted", err)
	}
	if got := lastAssistantContent(a.logSession); got != closing {
		t.Fatalf("persisted answer = %q, want the abort's closing text alone %q", got, closing)
	}
}

// Only the first round refused for want of an audit is kept. A later one (the
// model wrote text but did not call confirm_audit) is superseded as before,
// and the answer survives a textless closing round.
func TestScheduledOnlyTheFirstAuditRefusedRoundIsKept(t *testing.T) {
	a := newTestScheduledAgent(t, &itMockModel{})
	p := &scheduledPolicy{
		inner:  agentcore.NewScheduledPolicy(a.logSession, 50, 0, 0),
		agent:  a,
		task:   "Compute the value of every seed and answer with a table.",
		runCtx: context.Background(),
	}

	p.SetRoundFinalText(answerTable)
	ok, msgs := p.CanFinish(0)
	if ok || !strings.Contains(strings.Join(msgs, " "), auditAnswerNote) {
		t.Fatalf("the audit request must refuse and carry the note: ok=%v msgs=%q", ok, msgs)
	}

	p.SetRoundFinalText("I checked everything.")
	ok, msgs = p.CanFinish(1)
	if ok || strings.Contains(strings.Join(msgs, " "), auditAnswerNote) {
		t.Fatalf("an unconfirmed audit must refuse without the note: ok=%v msgs=%q", ok, msgs)
	}

	if got, want := p.RunAnswer(auditPointer), answerTable+"\n\n"+auditPointer; got != want {
		t.Errorf("run answer = %q, want the pre-audit answer and the closing text %q", got, want)
	}
	if got := p.RunAnswer(""); got != answerTable {
		t.Errorf("a textless closing round = %q, want the pre-audit answer %q", got, answerTable)
	}
}

// On a task with an output schema whose pre-audit answer is one JSON value, a
// supplement appended after it would be a second value. There the post-audit
// text replaces the answer, and the audit request asks for the whole answer.
func TestScheduledStructuredAnswerBeforeAuditIsReplaced(t *testing.T) {
	a := newTestScheduledAgent(t, &itMockModel{})
	a.outputSchema = json.RawMessage(`{"type":"object"}`)
	p := &scheduledPolicy{
		inner:  agentcore.NewScheduledPolicy(a.logSession, 50, 0, 0),
		agent:  a,
		task:   "Answer with one JSON object.",
		runCtx: context.Background(),
	}

	p.SetRoundFinalText(`{"values":[42,17]}`)
	ok, msgs := p.CanFinish(0)
	nudge := strings.Join(msgs, " ")
	if ok || !strings.Contains(nudge, reviewerRepairAnswerNote) || strings.Contains(nudge, auditAnswerNote) {
		t.Fatalf("a structured answer's audit request must ask for the whole answer: ok=%v msgs=%q", ok, msgs)
	}
	if got := p.RunAnswer(`{"values":[42,18]}`); got != `{"values":[42,18]}` {
		t.Errorf("run answer = %q, want the post-audit object alone", got)
	}
	if got := p.RunAnswer(""); got != `{"values":[42,17]}` {
		t.Errorf("a textless closing round = %q, want the pre-audit object", got)
	}
}

// enforcementLogged reports whether a system_enforcement message in the
// session log contains text.
func enforcementLogged(session *LogSession, text string) bool {
	for _, m := range session.SnapshotMessages() {
		if m.MessageType != nil && *m.MessageType == "system_enforcement" && strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}
