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
	abortedAudit = `{"success":false,"reasoning":"two values could not be verified","artifacts_checked":["values"],"workflow_sections_checked":["completion"],"send_contract_checked":false,"attachments_checked":[],"remaining_risks":["unverified values"],"user_visible_summary":"Two values could not be verified."}`
	seedTask     = "Compute the value of every seed and answer with a table."
)

// scriptedStep is one model call: a confirm_audit call with audit as its
// input, or else a closing message with text (none when empty).
type scriptedStep struct{ audit, text string }

// scriptedModel answers call i with steps[i] (the last step repeats) and
// records the prompt of every call.
func scriptedModel(prompts *[]string, steps ...scriptedStep) *itMockModel {
	calls := 0
	return &itMockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		step := steps[min(calls, len(steps)-1)]
		calls++
		raw, _ := json.Marshal(call.Prompt)
		*prompts = append(*prompts, string(raw))
		return func(yield func(fantasy.StreamPart) bool) {
			if step.audit != "" {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: step.audit})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			if step.text != "" {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: step.text})
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
}

// answerThenAudit is the #1826 shape: the first round closes with answer (no
// tool call, so the finish enforcement asks for the audit), the next calls
// confirm_audit with audit, and the run closes with closing.
func answerThenAudit(prompts *[]string, answer, audit, closing string) *itMockModel {
	return scriptedModel(prompts, scriptedStep{text: answer}, scriptedStep{audit: audit}, scriptedStep{text: closing})
}

// The issue's reproduction: the answer, then an audit round that only points
// back to it. The run's persisted answer is the table followed by the audit's
// closing text, and the audit request told the model that is how it composes.
func TestScheduledAnswerBeforeAuditIsKept(t *testing.T) {
	var prompts []string
	a := newTestScheduledAgent(t, answerThenAudit(&prompts, answerTable, passedAudit, auditPointer))
	if err := a.Execute(context.Background(), seedTask); err != nil {
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
	a := newTestScheduledAgent(t, answerThenAudit(&prompts, answerTable, passedAudit, auditPointer))
	a.verifierModel = verifier
	if err := a.Execute(context.Background(), seedTask); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if len(verifier.prompts) != 1 {
		t.Fatalf("verifier calls = %d, want 1", len(verifier.prompts))
	}
	if !strings.Contains(verifier.prompts[0], "| 1 | 42 |") || !strings.Contains(verifier.prompts[0], "previous message") {
		t.Errorf("the verifier must judge the pre-audit answer and the audit's closing text: %s", verifier.prompts[0])
	}
}

// A verifier repair after the audit composes with the kept answer like any
// judged answer: the repair round's supplement follows the table and the
// audit's closing text.
func TestScheduledVerifierRepairComposesWithTheAnswerBeforeAudit(t *testing.T) {
	var prompts []string
	const supplement = "| 3 | 4898 |"
	verifier := &recordingVerifier{replies: []string{
		`{"missing_actions":["report seed 3"],"reasoning":"seed 3 is missing"}`,
		`{"missing_actions":[],"reasoning":"every seed is reported"}`,
	}}
	a := newTestScheduledAgent(t, scriptedModel(&prompts,
		scriptedStep{text: answerTable}, scriptedStep{audit: passedAudit}, scriptedStep{text: auditPointer}, scriptedStep{text: supplement}))
	a.verifierModel = verifier
	if err := a.Execute(context.Background(), seedTask); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if want := answerTable + "\n\n" + auditPointer + "\n\n" + supplement; lastAssistantContent(a.logSession) != want {
		t.Fatalf("persisted answer = %q, want %q", lastAssistantContent(a.logSession), want)
	}
}

// Once a gate has judged the composition, the kept answer is part of what the
// gate judged: an abort after a verifier repair keeps that judged answer, as
// it would any judged answer. Only an answer no gate has judged yet is dropped
// by an abort.
func TestScheduledAbortAfterAGateKeepsTheJudgedAnswer(t *testing.T) {
	var prompts []string
	const closing = "Aborted: seed 3 could not be computed."
	verifier := &recordingVerifier{replies: []string{`{"missing_actions":["report seed 3"],"reasoning":"seed 3 is missing"}`}}
	a := newTestScheduledAgent(t, scriptedModel(&prompts,
		scriptedStep{text: answerTable}, scriptedStep{audit: passedAudit}, scriptedStep{text: auditPointer},
		scriptedStep{audit: abortedAudit}, scriptedStep{text: closing}))
	a.verifierModel = verifier
	if err := a.Execute(context.Background(), seedTask); !errors.Is(err, agentcore.ErrAuditAborted) {
		t.Fatalf("run returned %v, want ErrAuditAborted", err)
	}
	if want := answerTable + "\n\n" + auditPointer + "\n\n" + closing; lastAssistantContent(a.logSession) != want {
		t.Fatalf("persisted answer = %q, want the judged answer and the abort's text %q", lastAssistantContent(a.logSession), want)
	}
}

// A verifier that returns no valid verdict also sends the run back, and its
// repair composes with the answer as it stood, like any judged answer (#1675).
// So the kept text is part of that answer, and an abort after it keeps it.
func TestScheduledAbortAfterAVerifierErrorKeepsTheJudgedAnswer(t *testing.T) {
	withFastVerifierRetry(t)
	var prompts []string
	const closing = "Aborted: the values could not be verified."
	verifier := &recordingVerifier{replies: []string{"not a verdict"}}
	a := newTestScheduledAgent(t, scriptedModel(&prompts,
		scriptedStep{text: answerTable}, scriptedStep{audit: passedAudit}, scriptedStep{text: auditPointer},
		scriptedStep{audit: abortedAudit}, scriptedStep{text: closing}))
	a.verifierModel = verifier
	if err := a.Execute(context.Background(), seedTask); !errors.Is(err, agentcore.ErrAuditAborted) {
		t.Fatalf("run returned %v, want ErrAuditAborted", err)
	}
	if len(verifier.prompts) != 2 {
		t.Fatalf("verifier calls = %d, want 2 (the call and its one retry)", len(verifier.prompts))
	}
	if want := answerTable + "\n\n" + auditPointer + "\n\n" + closing; lastAssistantContent(a.logSession) != want {
		t.Fatalf("persisted answer = %q, want the judged answer and the abort's text %q", lastAssistantContent(a.logSession), want)
	}
}

// A model that audits on its own first, has that confirm_audit refused, and
// then answers without retrying is refused with "Audit not confirmed", not
// with the audit request. Its answer is kept the same way.
func TestScheduledAnswerAfterARefusedOwnAuditIsKept(t *testing.T) {
	const misnamed = `{"success":true,"critical_actions":[{"tool":"send_email"}],"reasoning":"checked every value","artifacts_checked":["values"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
	var prompts []string
	a := newTestScheduledAgent(t, scriptedModel(&prompts,
		scriptedStep{audit: misnamed}, scriptedStep{text: answerTable}, scriptedStep{audit: passedAudit}, scriptedStep{text: auditPointer}))
	if err := a.Execute(context.Background(), seedTask); err != nil {
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

// A correction written in a later refused round (the model audited, fixed a
// value, and ended the round without calling confirm_audit) follows the
// answer it corrects. Dropping it stored the uncorrected table under a closing
// text that called it corrected.
func TestScheduledCorrectionBeforeTheAuditPassesIsKept(t *testing.T) {
	var prompts []string
	const correction = "Correction: seed 2 is 18, not 17."
	const closing = "Audit confirmed; the corrected table above is final."
	a := newTestScheduledAgent(t, scriptedModel(&prompts,
		scriptedStep{text: answerTable}, scriptedStep{text: correction}, scriptedStep{audit: passedAudit}, scriptedStep{text: closing}))
	if err := a.Execute(context.Background(), seedTask); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if len(prompts) != 4 {
		t.Fatalf("model calls = %d, want 4 (answer, correction, audit, closing)", len(prompts))
	}
	if !strings.Contains(prompts[2], "Audit not confirmed.") || !strings.Contains(prompts[2], auditAnswerNote) {
		t.Errorf("the refusal after the correction must say it is kept too: %s", prompts[2])
	}
	if want := answerTable + "\n\n" + correction + "\n\n" + closing; lastAssistantContent(a.logSession) != want {
		t.Fatalf("persisted answer = %q, want the answer, its correction and the closing text %q", lastAssistantContent(a.logSession), want)
	}
}

// A model that restates its whole answer after the audit, as the note asks it
// not to, still gets it stored once.
func TestScheduledAnswerBeforeAuditRestatedIsStoredOnce(t *testing.T) {
	var prompts []string
	restated := "Audit passed; the values are final.\n\n" + answerTable
	a := newTestScheduledAgent(t, answerThenAudit(&prompts, answerTable, passedAudit, restated))
	if err := a.Execute(context.Background(), seedTask); err != nil {
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
	a := newTestScheduledAgent(t, answerThenAudit(&prompts, "", passedAudit, answerTable))
	if err := a.Execute(context.Background(), seedTask); err != nil {
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
	const closing = "Aborted: two of the values could not be verified."
	a := newTestScheduledAgent(t, answerThenAudit(&prompts, "All done.\n\n"+answerTable, abortedAudit, closing))
	err := a.Execute(context.Background(), seedTask)
	if !errors.Is(err, agentcore.ErrAuditAborted) {
		t.Fatalf("aborted run returned %v, want ErrAuditAborted", err)
	}
	if got := lastAssistantContent(a.logSession); got != closing {
		t.Fatalf("persisted answer = %q, want the abort's closing text alone %q", got, closing)
	}
}

// Every round refused for want of the audit composes in, each refusal after
// a reply carries the note, a textless refused round adds nothing, and the
// kept text survives a textless closing round.
func TestScheduledEveryAuditRefusedRoundComposes(t *testing.T) {
	a := newTestScheduledAgent(t, &itMockModel{})
	p := &scheduledPolicy{
		inner:  agentcore.NewScheduledPolicy(a.logSession, 50, 0, 0),
		agent:  a,
		task:   seedTask,
		runCtx: context.Background(),
	}
	const correction = "Correction: seed 2 is 18, not 17."
	for i, round := range []struct {
		text string
		note bool
	}{{answerTable, true}, {"", false}, {correction, true}} {
		p.SetRoundFinalText(round.text)
		ok, msgs := p.CanFinish(i)
		if ok || strings.Contains(strings.Join(msgs, " "), auditAnswerNote) != round.note {
			t.Fatalf("round %d: ok=%v, want a refusal with note=%v: %q", i, ok, round.note, msgs)
		}
	}

	kept := answerTable + "\n\n" + correction
	if got, want := p.RunAnswer(auditPointer), kept+"\n\n"+auditPointer; got != want {
		t.Errorf("run answer = %q, want the kept text and the closing text %q", got, want)
	}
	if got := p.RunAnswer(""); got != kept {
		t.Errorf("a textless closing round = %q, want the kept text %q", got, kept)
	}
}

// On a task with an output schema whose pre-audit answer is one JSON value, a
// supplement appended after it would be a second value. There the post-audit
// text replaces the answer, and the audit request asks for the whole answer
// without calling the first one wrong.
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
	if ok || !strings.Contains(nudge, auditReplaceNote) || strings.Contains(nudge, auditAnswerNote) {
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
