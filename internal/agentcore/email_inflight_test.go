package agentcore

// Tests for the send_email duplicate guard's two markers: "already sent"
// (recorded only when the provider accepted the send) and "in flight"
// (held from admission until the call's result is recorded). A send that
// fails must leave neither behind, so an identical retry in the same turn
// goes out; a send that succeeded, or one still running, must keep an
// identical call from sending twice, sequentially or in parallel.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

const (
	inflightEmailTool  = "mcp_ses_send_email"
	inflightEmailInput = `{"to_email":"ops@example.com","subject":"weekly report","content":"<p>numbers</p>"}`
	emailAccepted      = `{"status_code":202}`
	alreadySentText    = "identical send_email payload already sent"
	stillRunningText   = "already running"
)

// emailFailures are the ways a send comes back provably undelivered: a
// request that never reached the server, a call refused before dispatch, and
// a provider that answered with a rejection.
var emailFailures = []struct {
	name      string
	result    string
	succeeded bool
}{
	{"request not delivered", mcpCallErrorPrefix(inflightEmailTool, true) + "tool call failed after server restart: stdio write failed (request not delivered)", false},
	{"not dispatched", mcpCallNotSentPrefix + "context deadline exceeded", false},
	{"provider rejected", `{"status_code":400,"error":"invalid recipient"}`, true},
	{"is-error answer", `{"success":false,"error":"invalid recipient"}`, false},
}

// restartAlsoFailedText is the error of a stdio server that died after
// reading the request and whose restart then failed writing its initialize:
// the "request not delivered" in it is the restart's, not the tool request's.
const restartAlsoFailedText = "tool call failed and server restart also failed: original=EOF, restart=failed to reinitialize server: " +
	"initialize call failed: stdio write failed (request not delivered): write |1: broken pipe"

// emailUnknownOutcomes are the ways a send comes back without saying whether
// the provider took the message.
var emailUnknownOutcomes = []struct {
	name      string
	result    string
	succeeded bool
}{
	{"timeout after dispatch", "Error calling " + inflightEmailTool + ": context deadline exceeded", false},
	{"server died mid-call", "Error calling " + inflightEmailTool + ": MCP server ses died while executing send_email and was restarted; the call's outcome is UNKNOWN — verify whether the action took effect: EOF", false},
	{"connection dropped", "Error calling " + inflightEmailTool + ": read tcp 10.0.0.2:5432: connection reset by peer", false},
	{"server reports unknown outcome", `{"success":false,"error_code":"ses_outcome_unknown"}`, true},
	{"contained panic", containedToolPanicText("inc-123"), false},
	{"server died, restart failed too", mcpCallErrorPrefix(inflightEmailTool, false) + restartAlsoFailedText, false},
	{"provider 500", `{"status_code":500,"error":"internal error"}`, true},
	{"gateway 502", `{"status_code":502,"error":"bad gateway"}`, true},
	{"gateway timeout 504", `{"status_code":504}`, true},
	{"provider read timeout", `Read timeout on endpoint URL: "https://email.us-east-1.amazonaws.com/"`, false},
}

const maybeSentText = "it may have been sent"

func mustAdmit(t *testing.T, p Policy, step string) {
	t.Helper()
	if blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-"+step, inflightEmailInput); blocked {
		t.Fatalf("%s: identical send refused (%q), want it to go out", step, msg)
	}
}

func mustRefuse(t *testing.T, p Policy, step, want string) {
	t.Helper()
	blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-"+step, inflightEmailInput)
	if !blocked || !strings.Contains(msg, want) {
		t.Fatalf("%s: got blocked=%v msg=%q, want a refusal containing %q", step, blocked, msg, want)
	}
}

// The interactive admissions that let a send run without a card: session
// pre-approval, an Autonomous self-addressed send (durable receipt), and a
// transport with no approval UI at all.
func interactiveAdmissions() map[string]func() *InteractivePolicy {
	return map[string]func() *InteractivePolicy{
		"pre-approved": func() *InteractivePolicy {
			return NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
		},
		"autonomous recorded": func() *InteractivePolicy {
			return NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: RecordedSentinel}, nil)
		},
		"no approval sink": func() *InteractivePolicy {
			return NewInteractivePolicy(0, 0, nil, nil)
		},
	}
}

func TestInteractiveEmail_FailedSendCanBeRetriedIdentically(t *testing.T) {
	for admission, newPolicy := range interactiveAdmissions() {
		for _, failure := range emailFailures {
			t.Run(admission+"/"+failure.name, func(t *testing.T) {
				p := newPolicy()
				mustAdmit(t, p, "first send")
				p.RecordToolResult(inflightEmailTool, inflightEmailInput, failure.result, failure.succeeded)

				// Before the fix the payload was marked sent at admission, so
				// this retry was refused as "already sent" although nothing
				// had been delivered.
				mustAdmit(t, p, "retry after failure")
				p.RecordToolResult(inflightEmailTool, inflightEmailInput, emailAccepted, true)

				mustRefuse(t, p, "resend after success", alreadySentText)
				if got := p.orch.sendEmailSuccessCount; got != 1 {
					t.Fatalf("sendEmailSuccessCount = %d, want 1 (the failed send must not count)", got)
				}
			})
		}
	}
}

func TestInteractiveEmail_SuccessfulSendStillBlocksIdenticalResend(t *testing.T) {
	for admission, newPolicy := range interactiveAdmissions() {
		t.Run(admission, func(t *testing.T) {
			p := newPolicy()
			mustAdmit(t, p, "first send")
			p.RecordToolResult(inflightEmailTool, inflightEmailInput, emailAccepted, true)
			mustRefuse(t, p, "identical resend", alreadySentText)
		})
	}
}

func TestInteractiveEmail_IdenticalCallWhileFirstRunsIsRefusedNotReportedSent(t *testing.T) {
	for admission, newPolicy := range interactiveAdmissions() {
		t.Run(admission, func(t *testing.T) {
			p := newPolicy()
			mustAdmit(t, p, "first send")
			blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-second", inflightEmailInput)
			if !blocked || !strings.Contains(msg, stillRunningText) {
				t.Fatalf("second identical call while the first runs: blocked=%v msg=%q, want the in-flight refusal", blocked, msg)
			}
			// The model repeats this text to the user; it must not claim a
			// delivery that has not happened.
			if strings.Contains(msg, "already sent") {
				t.Fatalf("in-flight refusal %q claims the email was sent", msg)
			}
			// A different payload is not a duplicate of the running one.
			other := strings.Replace(inflightEmailInput, "numbers", "other numbers", 1)
			if blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-other", other); blocked {
				t.Fatalf("distinct payload refused while another send runs: %q", msg)
			}
		})
	}
}

// A staged card does not run the send in this turn, so it holds nothing in
// flight: a later identical call stages again rather than being refused as
// running.
func TestInteractiveEmail_StagedCardHoldsNothingInFlight(t *testing.T) {
	sink := &stubApprovalSink{returnID: "ap-1"}
	p := NewInteractivePolicy(0, 0, sink, nil)
	for i := range 2 {
		blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-stage", inflightEmailInput)
		if !blocked || !strings.Contains(msg, "APPROVAL_REQUIRED") {
			t.Fatalf("call %d: blocked=%v msg=%q, want a staged card", i+1, blocked, msg)
		}
	}
	if len(sink.staged) != 2 {
		t.Fatalf("staged %d cards, want 2", len(sink.staged))
	}
}

// auditedScheduledState is a scheduled run whose audit has passed and
// committed one send_email, so a send reaches the admission at the end of
// checkCriticalTool, and a failed send keeps the audit (the commitment is
// still owed) for the retry.
func auditedScheduledState() *orchestrationState {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{"send_email: weekly report to ops"})
	o.auditConfirmed = true
	return o
}

func TestScheduledEmail_FailedSendCanBeRetriedAndRunningOneBlocksDuplicate(t *testing.T) {
	for _, failure := range emailFailures {
		t.Run(failure.name, func(t *testing.T) {
			o := auditedScheduledState()
			if blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput); blocked {
				t.Fatalf("first send refused: %q", msg)
			}
			blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput)
			if !blocked || !strings.Contains(msg, stillRunningText) || strings.HasPrefix(msg, DuplicateSendSuppressedPrefix) {
				t.Fatalf("identical call while the first runs: blocked=%v msg=%q, want the in-flight refusal (no discharge)", blocked, msg)
			}
			o.recordToolResult(inflightEmailTool, inflightEmailInput, failure.result, failure.succeeded)

			if blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput); blocked {
				t.Fatalf("retry after a failed send refused: %q", msg)
			}
			o.recordToolResult(inflightEmailTool, inflightEmailInput, emailAccepted, true)
			blocked, msg = o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput)
			if !blocked || !strings.HasPrefix(msg, DuplicateSendSuppressedPrefix) {
				t.Fatalf("resend after success: blocked=%v msg=%q, want the already-sent suppression", blocked, msg)
			}
		})
	}
}

// A call the scheduled gate refuses (here: audit not confirmed) leaves no
// in-flight key, so once the audit passes the same payload is admitted.
func TestScheduledEmail_RefusedCallHoldsNothingInFlight(t *testing.T) {
	o := newOrchStateForTest()
	blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput)
	if !blocked || !strings.Contains(msg, "requires audit first") {
		t.Fatalf("blocked=%v msg=%q, want the audit gate", blocked, msg)
	}
	o.mu.Lock()
	o.auditConfirmed = true
	o.mu.Unlock()
	if blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput); blocked {
		t.Fatalf("send after the audit refused: %q", msg)
	}
}

// emailFraming is the shared tool-call framing around a send_email whose
// execute step is exec.
func emailFraming(p Policy, journal TurnJournal, validate func() (string, bool), exec func(context.Context) toolCallOutcome) toolCallFraming {
	return toolCallFraming{
		name:     inflightEmailTool,
		policy:   p,
		journal:  journal,
		validate: validate,
		execute:  exec,
	}
}

func emailCall(id string) fantasy.ToolCall {
	return fantasy.ToolCall{ID: id, Name: inflightEmailTool, Input: inflightEmailInput}
}

// Two identical send_email calls dispatched at the same time (what
// parallel_safe_tools allows) send once: the second is refused while the
// first is still out, and after the first succeeds an identical call is
// refused as already sent.
func TestEmailFraming_TwoIdenticalParallelCallsSendOnce(t *testing.T) {
	p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
	var sends atomic.Int32
	var startOnce sync.Once
	started := make(chan struct{})
	finish := make(chan struct{})
	exec := func(context.Context) toolCallOutcome {
		sends.Add(1)
		startOnce.Do(func() { close(started) })
		// A regression that lets a second call through must fail the
		// count below, not hang here.
		select {
		case <-finish:
		case <-time.After(10 * time.Second):
		}
		return toolCallOutcome{resp: fantasy.NewTextResponse(emailAccepted)}
	}
	f := emailFraming(p, nil, nil, exec)

	go1 := make(chan fantasy.ToolResponse, 1)
	go func() {
		resp, _ := runGovernedToolCall(context.Background(), f, emailCall("c-1"))
		go1 <- resp
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("first call never reached the provider")
	}
	// The first call is inside the provider; the identical second call
	// arrives now and must not reach execute.
	resp2, _ := runGovernedToolCall(context.Background(), f, emailCall("c-2"))
	close(finish)
	if !resp2.IsError || !strings.Contains(resp2.Content, stillRunningText) {
		t.Fatalf("parallel identical call: %+v, want the in-flight refusal", resp2)
	}
	select {
	case resp1 := <-go1:
		if resp1.IsError {
			t.Fatalf("first call failed: %q", resp1.Content)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first call never returned")
	}

	resp3, _ := runGovernedToolCall(context.Background(), f, emailCall("c-3"))
	if !resp3.IsError || !strings.Contains(resp3.Content, alreadySentText) {
		t.Fatalf("identical call after success: %+v, want the already-sent refusal", resp3)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("provider saw %d sends, want 1", got)
	}
}

// Many identical calls racing through the gates at once: exactly one runs.
func TestEmailFraming_RacingIdenticalCallsAdmitOne(t *testing.T) {
	p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			if blocked, _ := p.orch.checkEmailSafety(inflightEmailTool, "c-race-"+string(rune('a'+i)), inflightEmailInput); !blocked {
				admitted.Add(1)
			}
		}()
	}
	close(gate)
	wg.Wait()
	if got := admitted.Load(); got != 1 {
		t.Fatalf("%d identical racing calls admitted, want exactly 1", got)
	}
}

type failingIntentJournal struct{ fail bool }

func (j *failingIntentJournal) ToolIntent(context.Context, string, string, string) error {
	if j.fail {
		return errors.New("journal unavailable")
	}
	return nil
}

func (j *failingIntentJournal) ToolOutcome(context.Context, string, string, string, bool) error {
	return nil
}

// A send the gates admitted but the framing then refused before it ran (a
// failed intent-journal write, or the tool's own argument check) never
// records a result, so the framing must hand its in-flight key back: an
// identical retry once the journal recovers has to go out.
func TestEmailFraming_PreDispatchRefusalReleasesInFlight(t *testing.T) {
	cases := []struct {
		name     string
		journal  *failingIntentJournal
		validate func(fail *bool) func() (string, bool)
	}{
		{name: "intent journal write fails", journal: &failingIntentJournal{fail: true}},
		{name: "argument check refuses", validate: func(fail *bool) func() (string, bool) {
			return func() (string, bool) {
				if *fail {
					return "invalid arguments", false
				}
				return "", true
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
			var sends atomic.Int32
			exec := func(context.Context) toolCallOutcome {
				sends.Add(1)
				return toolCallOutcome{resp: fantasy.NewTextResponse(emailAccepted)}
			}
			failValidate := true
			var validate func() (string, bool)
			if tc.validate != nil {
				validate = tc.validate(&failValidate)
			}
			var journal TurnJournal
			if tc.journal != nil {
				journal = tc.journal
			}
			f := emailFraming(p, journal, validate, exec)

			resp, _ := runGovernedToolCall(context.Background(), f, emailCall("c-1"))
			if !resp.IsError || sends.Load() != 0 {
				t.Fatalf("first call: %+v sends=%d, want a pre-dispatch refusal", resp, sends.Load())
			}
			if tc.journal != nil {
				tc.journal.fail = false
			}
			failValidate = false
			resp, _ = runGovernedToolCall(context.Background(), f, emailCall("c-2"))
			if resp.IsError || sends.Load() != 1 {
				t.Fatalf("retry after the refusal: %+v sends=%d, want it sent", resp, sends.Load())
			}
		})
	}
}

// A send the server reports as successful without a status_code is a sent
// email too: an identical resend is refused, in chat and in a scheduled run.
func TestEmail_SuccessWithoutStatusCodeBlocksIdenticalResend(t *testing.T) {
	const ok = `{"message_id":"m-1","queued":true}`
	for admission, newPolicy := range interactiveAdmissions() {
		t.Run(admission, func(t *testing.T) {
			p := newPolicy()
			mustAdmit(t, p, "first send")
			p.RecordToolResult(inflightEmailTool, inflightEmailInput, ok, true)
			mustRefuse(t, p, "identical resend", alreadySentText)
		})
	}
	t.Run("scheduled", func(t *testing.T) {
		o := auditedScheduledState()
		if blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput); blocked {
			t.Fatalf("first send refused: %q", msg)
		}
		o.recordToolResult(inflightEmailTool, inflightEmailInput, ok, true)
		blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput)
		if !blocked || !strings.HasPrefix(msg, DuplicateSendSuppressedPrefix) {
			t.Fatalf("identical resend: blocked=%v msg=%q, want the already-sent suppression", blocked, msg)
		}
	})
}

// A send whose outcome is unknown may have reached the provider: an identical
// call in the same turn is refused with a text that says so and tells the
// model how to proceed, never as "already sent" and never sent again. A new
// turn starts without the record.
func TestInteractiveEmail_UnknownOutcomeRefusesIdenticalSend(t *testing.T) {
	for admission, newPolicy := range interactiveAdmissions() {
		for _, unknown := range emailUnknownOutcomes {
			t.Run(admission+"/"+unknown.name, func(t *testing.T) {
				p := newPolicy()
				mustAdmit(t, p, "first send")
				p.RecordToolResult(inflightEmailTool, inflightEmailInput, unknown.result, unknown.succeeded)

				blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-retry", inflightEmailInput)
				if !blocked || !strings.Contains(msg, maybeSentText) || !strings.Contains(msg, "new message") {
					t.Fatalf("identical send after an unknown outcome: blocked=%v msg=%q, want the may-have-been-sent refusal", blocked, msg)
				}
				if strings.Contains(msg, alreadySentText) {
					t.Fatalf("refusal %q claims the email was sent", msg)
				}
				// A different payload is unaffected.
				other := strings.Replace(inflightEmailInput, "numbers", "other numbers", 1)
				if blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-other", other); blocked {
					t.Fatalf("distinct payload refused: %q", msg)
				}
				// The next turn has a fresh policy and so a clean record.
				mustAdmit(t, newPolicy(), "next turn")
			})
		}
	}
}

func TestScheduledEmail_UnknownOutcomeRefusesWithoutDischarge(t *testing.T) {
	for _, unknown := range emailUnknownOutcomes {
		t.Run(unknown.name, func(t *testing.T) {
			o := auditedScheduledState()
			if blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput); blocked {
				t.Fatalf("first send refused: %q", msg)
			}
			o.recordToolResult(inflightEmailTool, inflightEmailInput, unknown.result, unknown.succeeded)
			blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput)
			if !blocked || !strings.Contains(msg, maybeSentText) || !strings.Contains(msg, "confirm_audit(success=false") {
				t.Fatalf("identical send after an unknown outcome: blocked=%v msg=%q, want the may-have-been-sent refusal", blocked, msg)
			}
			o.mu.Lock()
			owed := o.unexecutedCommitments()
			o.mu.Unlock()
			if len(owed) == 0 {
				t.Fatal("the refusal discharged the send's commitment; whether it went out is not known")
			}
		})
	}
}

// Every gate after the send gate passes a send_email through. The send gate
// may already have recorded a durable receipt for the send by then, so a
// later refusal would leave a receipt for an email that never went; this
// pins the chain's order for each kind of approval answer.
func TestGateChains_LaterGatesPassEmailThrough(t *testing.T) {
	sinks := map[string]ApprovalStager{
		"recorded":     &stubApprovalSink{returnID: RecordedSentinel},
		"pre-approved": &stubApprovalSink{returnID: PreApprovedSentinel},
		"staged card":  &stubApprovalSink{returnID: "ap-1"},
		"no sink":      nil,
	}
	tools := []string{"send_email", inflightEmailTool}
	for sinkName, sink := range sinks {
		o := newOrchStateForTest()
		if sink != nil {
			o.setApprovalSink(sink)
		}
		for chainName, gates := range map[string][]toolGate{
			"interactive": o.interactiveGates(),
			"scheduled":   o.scheduledGates(),
		} {
			sendGate := -1
			for i, g := range gates {
				if g.admitsSend {
					if sendGate >= 0 {
						t.Fatalf("%s chain has two send gates", chainName)
					}
					sendGate = i
				}
			}
			if sendGate < 0 {
				t.Fatalf("%s chain has no send gate", chainName)
			}
			for _, g := range gates[sendGate+1:] {
				for _, tool := range tools {
					if blocked, msg := g.check(tool, "c-1", inflightEmailInput); blocked {
						t.Errorf("%s/%s: gate %q after the send gate refused %s: %q", chainName, sinkName, g.name, tool, msg)
					}
				}
			}
		}
	}
}

// Should a gate after the send gate refuse anyway, the payload the send gate
// held in flight is handed back, so an identical retry is not refused as
// "already running" for the rest of the turn.
func TestGateChain_LaterRefusalReleasesInFlight(t *testing.T) {
	p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
	o := p.orch
	gates := []toolGate{
		{name: "email", check: o.checkEmailSafety, admitsSend: true},
		{name: "refuses", check: func(string, string, string) (bool, string) { return true, "no" }},
	}
	if blocked, _ := runGateChain(o, gates, inflightEmailTool, "c-1", inflightEmailInput); !blocked {
		t.Fatal("the refusing gate did not refuse")
	}
	mustAdmit(t, p, "retry after a later refusal")
}

// The policy reads an MCP send's delivery from mcpTool's error prefix, which
// comes from mcp.RequestNotSent, never from phrases in the error text: a
// server that died after reading the request, whose restart then failed
// writing its own initialize, says "request not delivered" — about the
// restart — and must still count as a send that may have gone out.
func TestMCPEmail_DeliveryComesFromTheMarkNotTheText(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantRetry bool
	}{
		{"server died after reading, restart failed", errors.New(restartAlsoFailedText), false},
		{"request never left", mcp.MarkNotSent(errors.New("stdio write failed (request not delivered): broken pipe")), true},
		{"stopped before send", mcp.ErrStoppedBeforeSend, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewInteractivePolicy(0, 0, &stubApprovalSink{returnID: PreApprovedSentinel}, nil)
			broker := &recordingBroker{err: tc.err}
			tool := &mcpTool{serverName: "ses", tool: mcp.Tool{Name: "send_email"}, broker: broker, policy: p}
			if _, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c-1", Input: inflightEmailInput}); err != nil {
				t.Fatal(err)
			}
			if broker.calls != 1 {
				t.Fatalf("broker calls = %d, want 1", broker.calls)
			}
			blocked, msg := p.BeforeToolCall(inflightEmailTool, "c-2", inflightEmailInput)
			if tc.wantRetry && blocked {
				t.Fatalf("retry after a send that never left refused: %q", msg)
			}
			if !tc.wantRetry && (!blocked || !strings.Contains(msg, maybeSentText)) {
				t.Fatalf("retry after a send that may have gone out: blocked=%v msg=%q, want the may-have-been-sent refusal", blocked, msg)
			}
		})
	}
}

// The may-have-been-sent refusal warns off a disguised resend, as the
// already-sent suppression does.
func TestMaybeSentRefusal_ForbidsRewording(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		if msg := maybeSentEmailRefusal(scheduled); !strings.Contains(msg, "do NOT alter the content to make it look different") {
			t.Errorf("scheduled=%v: %q", scheduled, msg)
		}
	}
}

// A scheduled run that still owes a send_email after a send ended with an
// unknown outcome is not told to "execute" the email again — which would push
// a reworded second copy past the duplicate guard — but to abort saying the
// delivery is in doubt.
func TestScheduledEmail_FinishDoesNotRedemandAnEmailInDoubt(t *testing.T) {
	o := auditedScheduledState()
	o.selfAuditRequested, o.selfAuditConfirmedOnce = true, true
	if blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput); blocked {
		t.Fatalf("send refused: %q", msg)
	}
	o.recordToolResult(inflightEmailTool, inflightEmailInput, mcpCallErrorPrefix(inflightEmailTool, false)+"context deadline exceeded", false)

	ok, msgs := o.checkFinishEnforcement()
	if ok || len(msgs) != 1 {
		t.Fatalf("finish = (%v, %v), want one refusal", ok, msgs)
	}
	if !strings.Contains(msgs[0], "may already have been delivered") || !strings.Contains(msgs[0], "do NOT alter its content") ||
		!strings.Contains(msgs[0], "confirm_audit(success=false") || strings.Contains(msgs[0], "Execute each") {
		t.Fatalf("finish nudge %q, want the email-in-doubt nudge", msgs[0])
	}
}

// An aborted email-last run whose summary email ended with an unknown outcome
// is not told its reader "received NOTHING" and pushed to send another.
func TestEmailLast_AbortWithSummaryInDoubtDoesNotDemandAnother(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	confirmAudit(t, o, []criticalActionStruct{{Tool: elXandrCreate}, {Tool: elEmail}}, nil)
	mustNotBlock(t, o, elXandrCreate, elName("C"), elDefinitive)
	if blocked, msg := elCall(o, elEmail, elEmailArgs, mcpCallErrorPrefix(elEmail, false)+"context deadline exceeded", false); blocked {
		t.Fatalf("summary email refused: %s", msg)
	}
	if ok, msgs := elFinish(t, o); ok || len(msgs) == 0 || !strings.Contains(msgs[0], "may already have been delivered") {
		t.Fatalf("finish before the abort = (%v, %v), want the email-in-doubt nudge", ok, msgs)
	}
	confirmAuditAbort(t, o, "summary email outcome unknown")
	if ok, msgs := elFinish(t, o); !ok {
		t.Fatalf("finish after the abort refused with %v; the summary email may have reached the reader", msgs)
	}
}

// One send in doubt stands in for one owed email only: a run that owes three
// emails is still asked for the other two.
func TestScheduledEmail_FinishWaivesOnlyTheEmailsInDoubt(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{"send_email: report to ops", "send_email: report to sales", "send_email: report to finance"})
	o.auditConfirmed = true
	o.selfAuditRequested, o.selfAuditConfirmedOnce = true, true
	if blocked, msg := o.checkCriticalTool(inflightEmailTool, "", inflightEmailInput); blocked {
		t.Fatalf("send refused: %q", msg)
	}
	o.recordToolResult(inflightEmailTool, inflightEmailInput, mcpCallErrorPrefix(inflightEmailTool, false)+"context deadline exceeded", false)

	ok, msgs := o.checkFinishEnforcement()
	if ok || len(msgs) != 1 {
		t.Fatalf("finish = (%v, %v), want one refusal", ok, msgs)
	}
	if !strings.Contains(msgs[0], "may already have been delivered") ||
		!strings.Contains(msgs[0], "[send_email send_email]") ||
		!strings.Contains(msgs[0], "Send any other owed email whose content differs") {
		t.Fatalf("finish nudge %q, want the in-doubt email waived and the other two still demanded", msgs[0])
	}
}
