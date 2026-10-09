package agentcore

// Email-last batch rules (audit_email_last.go): settled-failed creates, the
// email-last gate from both sides, the abort notification allowance, deal_name
// binding and the re-audit restate. Ported from the v1 engine (cutlass #1087,
// #749.3, a4b76d83); the tool names are the DSP fixture's, installed with the
// two email-last policy lists on top (withEmailLastPolicy).

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

const (
	elIXCreate    = "mcp_indexexchange_mcp_ix_execute_deal_from_prompt_inputs"
	elXandrCreate = "mcp_xandr_mcp_xandr_execute_deal_from_prompt_inputs"
	elOXPrepared  = "mcp_openx_mcp_ox_create_prepared_deal"
	elOXPrepare   = "mcp_openx_mcp_ox_prepare_deal_from_prompt_inputs"
	elUpdate      = "mcp_magnite_mcp_magnite_update_deal"
	elEmail       = "mcp_sendgrid_send_email"

	elCreateOK  = `{"success":true,"deal_id":"D-OK"}`
	elEmailOK   = `{"status_code":202}`
	elEmailArgs = `{"to":["trader@example.com"],"subject":"Batch results","body":"sheet attached"}`
	// The 2026-10-05 Xandr failure: a permission error no argument change
	// can fix — definitive.
	elDefinitive = `{"success":false,"error_code":"xandr_line_item_create_failed",` +
		`"error":"UNAUTH: You are not authorized to set a margin on Curated Deal Line Items"}`
)

// withEmailLastPolicy installs the DSP fixture policy plus the email-last
// lists for one test, restoring the plain fixture afterwards.
func withEmailLastPolicy(t *testing.T) {
	t.Helper()
	p := testFixturePolicy()
	p.CriticalToolSuffixes = append(p.CriticalToolSuffixes, "update_deal")
	p.EmailLastToolSuffixes = []string{"update_deal"}
	p.SettleableCreateToolSuffixes = []string{"execute_deal_from_prompt_inputs", "create_prepared_deal", "create_deal"}
	ConfigureAgentPolicy(p)
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
}

// elCall runs one critical call through the gate and, when it is not blocked,
// records its result (transportOK=false is a transport error / is-error).
func elCall(o *orchestrationState, tool, args, result string, transportOK bool) (bool, string) {
	blocked, msg := o.checkCriticalTool(tool, "", args)
	if !blocked {
		o.recordToolResult(tool, args, result, transportOK)
	}
	return blocked, msg
}

func elName(name string) string { return `{"name":"` + name + `","dsp":"ttd"}` }

func mustNotBlock(t *testing.T, o *orchestrationState, tool, args, result string) {
	t.Helper()
	if blocked, msg := elCall(o, tool, args, result, true); blocked {
		t.Fatalf("%s %s must run, got blocked: %s", tool, args, msg)
	}
}

func mustBlock(t *testing.T, o *orchestrationState, tool, args, want string) {
	t.Helper()
	blocked, msg := elCall(o, tool, args, elEmailOK, true)
	if !blocked || !strings.Contains(msg, want) {
		t.Fatalf("%s must be blocked with %q, got blocked=%v msg=%s", tool, want, blocked, msg)
	}
}

// elFinish asks finish enforcement, with the self-audit ritual marked done
// (registerTyped arms commitments without running confirm_audit).
func elFinish(t *testing.T, o *orchestrationState) (bool, []string) {
	t.Helper()
	o.mu.Lock()
	o.selfAuditRequested, o.selfAuditConfirmedOnce = true, true
	o.mu.Unlock()
	return o.checkFinishEnforcement()
}

// (a) The 2026-10-05 shape: three creates, two book, the third fails
// definitively. The run sends ONE email reporting it and finishes, without an
// abort; the failed deal stays reported, not retried, and no deal write may
// follow the email.
func TestEmailLast_DefinitiveFailureSettlesAndRunEmailsOnce(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	resp := confirmAudit(t, o, []criticalActionStruct{
		{Tool: elIXCreate}, {Tool: elIXCreate}, {Tool: elXandrCreate}, {Tool: elEmail},
	}, nil)
	if resp.IsError {
		t.Fatalf("audit refused: %s", resp.Content)
	}
	mustNotBlock(t, o, elIXCreate, elName("A"), elCreateOK)
	mustNotBlock(t, o, elIXCreate, elName("B"), elCreateOK)
	// The Xandr create has not run yet: the email must wait for it.
	mustBlock(t, o, elEmail, elEmailArgs, "still unsettled")

	mustNotBlock(t, o, elXandrCreate, elName("C"), elDefinitive)
	if got := o.settledFailedSuffixes(); len(got) != 1 {
		t.Fatalf("the definitive failure must settle exactly one unit, got %v", got)
	}
	if ok, msgs := elFinish(t, o); ok {
		t.Fatalf("finish must wait for the summary email, got allowed (%v)", msgs)
	}
	mustNotBlock(t, o, elEmail, elEmailArgs, elEmailOK)
	if ok, msgs := elFinish(t, o); !ok {
		t.Fatalf("finish must be allowed once the email recorded the failure, got %v", msgs)
	}
	if o.auditTerminalFailure {
		t.Fatal("two of three deals booked: the run must not be recorded as aborted")
	}
	// The failed deal is reported, never retried after the email.
	mustBlock(t, o, elXandrCreate, elName("C"), "summary email has already been sent")
	if o.sendEmailSuccessCount != 1 {
		t.Fatalf("exactly one summary email, got %d", o.sendEmailSuccessCount)
	}
}

// (b) An ambiguous outcome — the deal MAY exist — never settles: the email
// stays blocked, and the run's exits are reconcile or abort (whose allowance
// still sends the one failure email). A definitive control settles.
func TestEmailLast_AmbiguousFailureNeverSettles(t *testing.T) {
	cases := []struct {
		name        string
		result      string
		transportOK bool
		settles     bool
	}{
		{"ambiguous transport code", `{"success":false,"error_code":"xandr_create_ambiguous_transport"}`, true, false},
		{"outcome unknown blocker", `{"success":false,"blockers":["pm_create_outcome_unknown"]}`, true, false},
		{"deal already created", `{"success":false,"error":"deal_already_created: exception after POST"}`, true, false},
		{"write state unknown", `{"success":false,"write_state":"unknown"}`, true, false},
		{"deal written", `{"success":false,"written":true,"error":"line item failed"}`, true, false},
		{"create call failed 5xx", `{"success":false,"error":"magnite_create_call_failed: 502 Bad Gateway"}`, true, false},
		{"fleet MCP restart: outcome unknown", "MCP server xandr died while executing x and was restarted; the call's outcome is UNKNOWN", false, false},
		{"fleet MCP request not delivered", "tool call failed after server restart: stdio write failed (request not delivered)", false, false},
		{"is-error result", `{"success":false,"error":"bad seat"}`, false, false},
		{"create call failed 4xx (definitive control)", `{"success":false,"error":"magnite_create_call_failed: HTTP 400 invalid floor"}`, true, true},
		{"permission error (definitive control)", elDefinitive, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withEmailLastPolicy(t)
			o := newOrchStateForTest()
			registerTyped(t, o, criticalActionStruct{Tool: elXandrCreate}, criticalActionStruct{Tool: elEmail})
			if blocked, msg := elCall(o, elXandrCreate, elName("C"), tc.result, tc.transportOK); blocked {
				t.Fatalf("create must run, got blocked: %s", msg)
			}
			blocked, msg := o.checkCriticalTool(elEmail, "", elEmailArgs)
			if tc.settles {
				if blocked {
					t.Fatalf("definitive failure must release the email, got: %s", msg)
				}
				return
			}
			if !blocked || !strings.Contains(msg, "still unsettled") {
				t.Fatalf("ambiguous outcome must keep the email blocked, got blocked=%v msg=%s", blocked, msg)
			}
		})
	}
}

// A definitive failure followed by an ambiguous retry of the SAME deal puts
// its unit back to unsettled ("latest attempt" semantics).
func TestEmailLast_AmbiguousRetryUnsettles(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: elXandrCreate}, criticalActionStruct{Tool: elEmail})
	mustNotBlock(t, o, elXandrCreate, elName("C"), elDefinitive)
	if blocked, msg := o.checkCriticalTool(elEmail, "", elEmailArgs); blocked {
		t.Fatalf("settled failure must release the email, got: %s", msg)
	}
	mustNotBlock(t, o, elXandrCreate, `{"name":"C","dsp":"ttd","floor":2}`, `{"success":false,"error_code":"xandr_create_ambiguous_transport"}`)
	mustBlock(t, o, elEmail, elEmailArgs, "still unsettled")
}

// (c) Abort after deal work: exactly ONE email passes with no new audit,
// finish is refused until it is sent, and the run keeps its failed verdict.
func TestEmailLast_AbortStillNotifiesOnce(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	confirmAudit(t, o, []criticalActionStruct{{Tool: elXandrCreate}, {Tool: elEmail}}, nil)
	mustNotBlock(t, o, elXandrCreate, elName("C"), `{"success":false,"error_code":"xandr_create_ambiguous_transport"}`)
	mustBlock(t, o, elEmail, elEmailArgs, "still unsettled")

	resp := confirmAuditAbort(t, o, "Xandr deal C outcome unknown; batch aborted")
	if resp.IsError || !strings.Contains(resp.Content, "send_email is unlocked for ONE failure-summary email") {
		t.Fatalf("abort must be accepted and name the email allowance, got %s", resp.Content)
	}
	if ok, msgs := elFinish(t, o); ok || len(msgs) == 0 || !strings.Contains(msgs[0], "received NOTHING") {
		t.Fatalf("finish must be refused until the failure email is sent, got ok=%v %v", ok, msgs)
	}
	mustNotBlock(t, o, elEmail, elEmailArgs, elEmailOK)
	// The allowance is ONE email: a second send falls back to the audit gate.
	mustBlock(t, o, elEmail, `{"to":["trader@example.com"],"subject":"again","body":"x"}`, "requires audit first")
	// No deal write after the email, even though the abort cleared the ledger.
	mustBlock(t, o, elXandrCreate, elName("C"), "summary email has already been sent")
	if ok, msgs := elFinish(t, o); !ok {
		t.Fatalf("finish must be allowed after the failure email, got %v", msgs)
	}
	if aborted, _, _ := o.auditVerdict(); !aborted {
		t.Fatal("the abort must stay the run's verdict")
	}
}

// The abort nudge is bounded: three refusals, then the run ends anyway.
func TestEmailLast_AbortNudgeIsBounded(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	confirmAudit(t, o, []criticalActionStruct{{Tool: elXandrCreate}, {Tool: elEmail}}, nil)
	mustNotBlock(t, o, elXandrCreate, elName("C"), `{"success":false,"error_code":"xandr_create_ambiguous_transport"}`)
	confirmAuditAbort(t, o, "aborted")
	for i := 1; i <= maxAbortNotifyNudges; i++ {
		if ok, _ := elFinish(t, o); ok {
			t.Fatalf("nudge %d: finish must still be refused", i)
		}
	}
	if ok, msgs := elFinish(t, o); !ok {
		t.Fatalf("after %d nudges finish must be allowed, got %v", maxAbortNotifyNudges, msgs)
	}
}

// An abort on a run that did no email-last work is unchanged: no allowance,
// no nudge.
func TestEmailLast_AbortWithoutBatchWorkUnchanged(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	confirmAudit(t, o, []criticalActionStruct{{Tool: elEmail}}, nil)
	confirmAuditAbort(t, o, "nothing to report")
	if ok, msgs := elFinish(t, o); !ok {
		t.Fatalf("finish must be allowed, got %v", msgs)
	}
	mustBlock(t, o, elEmail, elEmailArgs, "requires audit first")
}

// (d) Once the summary email has gone out, every email-last write is refused
// — an update included, and even under a fresh audit.
func TestEmailLast_DealWriteAfterEmailRefused(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	confirmAudit(t, o, []criticalActionStruct{{Tool: elUpdate, DealID: "5"}, {Tool: elEmail}}, nil)
	mustBlock(t, o, elEmail, elEmailArgs, "still unsettled")
	mustNotBlock(t, o, elUpdate, `{"deal_id":"5"}`, `{"success":true}`)
	mustNotBlock(t, o, elEmail, elEmailArgs, elEmailOK)

	resp := confirmAudit(t, o, []criticalActionStruct{{Tool: elUpdate, DealID: "6"}}, nil)
	if resp.IsError {
		t.Fatalf("re-audit refused: %s", resp.Content)
	}
	mustBlock(t, o, elUpdate, `{"deal_id":"6"}`, "summary email has already been sent")
}

// (e) deal_name on a typed critical_actions entry is ACCEPTED by the real
// confirm_audit decoder and binds each create unit to its deal.
func TestEmailLast_DealNameAcceptedAndBound(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	raw := `{"success":true,"reasoning":"checked","artifacts_checked":["brief.csv"],` +
		`"workflow_sections_checked":["build"],"send_contract_checked":true,"attachments_checked":[],` +
		`"remaining_risks":[],"critical_actions":[` +
		`{"tool":"` + elIXCreate + `","identifier":"row 1","deal_name":"Acme  OLV"},` +
		`{"tool":"` + elIXCreate + `","deal_name":"Acme CTV"},` +
		`{"tool":"` + elEmail + `"}]}`
	tool := buildConfirmAuditTool(o)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "a", Name: toolNameConfirmAudit, Input: raw})
	if err != nil || resp.IsError {
		t.Fatalf("confirm_audit with deal_name must be accepted, got err=%v resp=%s", err, resp.Content)
	}
	if !strings.Contains(resp.Content, `(deal name "acme olv")`) {
		t.Fatalf("the trailer must show the name-bound units, got %s", resp.Content)
	}

	// An undeclared name cannot ride a named unit.
	mustBlock(t, o, elIXCreate, elName("Acme Display"), "create_deal_name_not_declared")
	// The CTV deal fails definitively: only ITS unit settles.
	mustNotBlock(t, o, elIXCreate, elName("acme ctv"), elDefinitive)
	// The OLV deal is still open, so the email waits.
	mustBlock(t, o, elEmail, elEmailArgs, `"acme olv"`)
	mustNotBlock(t, o, elIXCreate, elName("ACME OLV"), elCreateOK)
	mustNotBlock(t, o, elEmail, elEmailArgs, elEmailOK)
	if ok, msgs := elFinish(t, o); !ok {
		t.Fatalf("finish must be allowed, got %v", msgs)
	}

	// A re-audit repeating the names restates them (registers nothing new).
	o2 := newOrchStateForTest()
	registerTyped(t, o2, criticalActionStruct{Tool: elIXCreate, DealName: "A"}, criticalActionStruct{Tool: elIXCreate, DealName: "B"})
	mustNotBlock(t, o2, elIXCreate, elName("A"), elCreateOK)
	if got := o2.registerCommittedActionsTyped([]criticalActionStruct{{Tool: elIXCreate, DealName: "a"}, {Tool: elIXCreate, DealName: "B"}}); got != 2 {
		t.Fatalf("restated audit must still be accepted (2 units accounted), got %d", got)
	}
	if got := o2.committedCriticalActions["execute_deal_from_prompt_inputs"]; got != 1 {
		t.Fatalf("restating the named batch must leave only B outstanding, got %d", got)
	}
}

// deal_name also binds a two-step create: the create carries only the
// prepared handle, which the prepare step tied to the name.
func TestEmailLast_PreparedHandleBindsToDealName(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: elOXPrepared, DealName: "OX One"}, criticalActionStruct{Tool: elOXPrepared, DealName: "OX Two"})
	o.recordToolResult(elOXPrepare, `{"name":"OX Two"}`, `{"success":true,"prepared_deal_id":"p-2"}`, true)
	mustNotBlock(t, o, elOXPrepared, `{"prepared_deal_id":"p-2"}`, elDefinitive)
	for _, c := range o.typedCommitments {
		if c.settledFailed != (c.dealName == "ox two") {
			t.Fatalf("only the OX Two unit may settle, got %s settled=%v", c.describe(), c.settledFailed)
		}
	}
}

// (f) A corrected retry of a settled-failed deal discharges THAT deal's unit;
// another deal's success never consumes a sibling's recorded failure, so a
// never-attempted deal cannot look settled.
func TestEmailLast_RetryAfterSettledFailureDischargesCleanly(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	registerTyped(t, o,
		criticalActionStruct{Tool: elIXCreate}, criticalActionStruct{Tool: elIXCreate},
		criticalActionStruct{Tool: elIXCreate}, criticalActionStruct{Tool: elEmail})

	mustNotBlock(t, o, elIXCreate, elName("A"), elDefinitive)
	mustNotBlock(t, o, elIXCreate, elName("B"), elCreateOK)
	// C was never attempted: B's success must not have consumed A's settled
	// unit, so one unsettled unit (C's) still holds the email.
	mustBlock(t, o, elEmail, elEmailArgs, "still unsettled")

	// A's corrected retry succeeds and discharges A's own settled unit.
	mustNotBlock(t, o, elIXCreate, `{"name":"A","dsp":"ttd","floor":3}`, elCreateOK)
	if got := o.settledFailedSuffixes(); len(got) != 0 {
		t.Fatalf("A's retry must discharge its own settled unit, still settled: %v", got)
	}
	mustBlock(t, o, elEmail, elEmailArgs, "still unsettled")
	mustNotBlock(t, o, elIXCreate, elName("C"), elCreateOK)
	mustNotBlock(t, o, elEmail, elEmailArgs, elEmailOK)
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("everything discharged, got outstanding %v", missing)
	}
	if ok, msgs := elFinish(t, o); !ok {
		t.Fatalf("finish must be allowed, got %v", msgs)
	}
}

// Re-audit restate (v1 9-deal run): re-declaring the batch after some deals
// booked adds no phantom units, keeps settled-failed state, and only entries
// beyond the batch's total register.
func TestEmailLast_ReauditRestatesBatch(t *testing.T) {
	withEmailLastPolicy(t)
	o := newOrchStateForTest()
	batch := []criticalActionStruct{{Tool: elIXCreate}, {Tool: elIXCreate}, {Tool: elIXCreate}, {Tool: elEmail}}
	registerTyped(t, o, batch...)
	mustNotBlock(t, o, elIXCreate, elName("A"), elCreateOK)
	mustNotBlock(t, o, elIXCreate, elName("B"), elCreateOK)
	mustNotBlock(t, o, elIXCreate, elName("C"), elDefinitive)

	if got := o.registerCommittedActionsTyped(batch); got == 0 {
		t.Fatal("a re-audit made of restated entries must still be accepted")
	}
	if got := o.committedCriticalActions["execute_deal_from_prompt_inputs"]; got != 1 {
		t.Fatalf("re-declaring the whole batch must leave 1 create outstanding, got %d", got)
	}
	if got := o.settledFailedSuffixes(); len(got) != 1 {
		t.Fatalf("the settled failure must survive the re-audit, got %v", got)
	}
	if blocked, msg := o.checkCriticalTool(elEmail, "", elEmailArgs); blocked {
		t.Fatalf("restated batch with a settled failure must release the email, got: %s", msg)
	}

	// The whole batch plus one more deal adds exactly one unit.
	plusOne := append([]criticalActionStruct{{Tool: elIXCreate}}, batch...)
	o.registerCommittedActionsTyped(plusOne)
	if got := o.committedCriticalActions["execute_deal_from_prompt_inputs"]; got != 2 {
		t.Fatalf("whole batch + 1 must add exactly one unit, got %d outstanding", got)
	}
}

// With no email-last lists (every bundle today) nothing changes: a failed
// create keeps its commitment open and the email is not ordered.
func TestEmailLast_InertWithoutPolicy(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: elXandrCreate}, criticalActionStruct{Tool: elEmail})
	mustNotBlock(t, o, elXandrCreate, elName("C"), elDefinitive)
	if got := o.settledFailedSuffixes(); len(got) != 0 {
		t.Fatalf("no settleable_create_tools: nothing may settle, got %v", got)
	}
	mustNotBlock(t, o, elEmail, elEmailArgs, elEmailOK)
	if ok, _ := elFinish(t, o); ok {
		t.Fatal("the failed create stays owed without the policy")
	}
}

func TestEmailLastPolicyProblems(t *testing.T) {
	problems := EmailLastPolicyProblems(AgentPolicy{
		CriticalToolSuffixes:         []string{"create_deal", "update_deal"},
		EmailLastToolSuffixes:        []string{"update_deal", "updat_deal", "send_email"},
		SettleableCreateToolSuffixes: []string{"create_deal", "clone_deal"},
	})
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`"updat_deal"`, `"send_email"`, `"clone_deal"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a problem naming %s, got %v", want, problems)
		}
	}
	if len(problems) != 3 {
		t.Errorf("want exactly 3 problems, got %v", problems)
	}
}
