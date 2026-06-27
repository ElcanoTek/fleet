package agentcore

// Lifted+adapted from cutlass orchestration_test.go (audit gating, batch
// commitments, retry budgets, repeated-call guard) plus the orchestration
// sub-tests from cutlass execute_test.go. No structural changes — the
// orchestrationState API is the cutlass superset.

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
)

func newOrchStateForTest() *orchestrationState {
	return newOrchestrationState(&LogSession{}, 100)
}

func TestBatchAudit_MultipleSameSuffixDeals(t *testing.T) {
	o := newOrchStateForTest()

	o.registerCommittedActions([]string{
		"create_deal: OpenX — AdGreetings_PG_25",
		"create_deal: OpenX — AdGreetings_PG_45",
		"create_deal: OpenX — AdGreetings_PG_60",
	})

	if got := o.committedCriticalActions["create_deal"]; got != 3 {
		t.Fatalf("expected 3 create_deal commitments, got %d", got)
	}
	if missing := o.unexecutedCommitments(); len(missing) != 3 {
		t.Fatalf("expected 3 unexecuted commitments, got %d (%v)", len(missing), missing)
	}

	o.auditConfirmed = true

	const tool = "mcp_openx_mcp_ox_create_deal"
	for i := 1; i <= 3; i++ {
		args := fmt.Sprintf(`{"deal":"d%d"}`, i)
		blocked, msg := o.checkCriticalTool(tool, "", args)
		if blocked {
			t.Fatalf("deal %d unexpectedly blocked: %s", i, msg)
		}
		o.recordToolResult(tool, args, "ok", true)

		switch i {
		case 1, 2:
			if !o.auditConfirmed {
				t.Fatalf("after deal %d audit token was consumed prematurely (commitments: %v)",
					i, o.committedCriticalActions)
			}
		case 3:
			if o.auditConfirmed {
				t.Fatal("after deal 3 audit token should have consumed but is still true")
			}
		}
	}

	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("expected no unexecuted commitments after 3 deals, got %v", missing)
	}

	blocked, _ := o.checkCriticalTool(tool, "", `{"x":1}`)
	if !blocked {
		t.Fatal("4th create_deal was not blocked despite exhausted audit")
	}
}

func TestBatchAudit_MixedSuffixes(t *testing.T) {
	o := newOrchStateForTest()

	o.registerCommittedActions([]string{
		"execute_deal_from_prompt_inputs: OpenX — D1",
		"execute_deal_from_prompt_inputs: IndexExchange — D2",
		"send_email: deal_sheet → trader@example.com",
	})

	if got := o.committedCriticalActions["execute_deal_from_prompt_inputs"]; got != 2 {
		t.Fatalf("expected 2 execute_deal_from_prompt_inputs commitments, got %d", got)
	}
	if got := o.committedCriticalActions["send_email"]; got != 1 {
		t.Fatalf("expected 1 send_email commitment, got %d", got)
	}

	o.auditConfirmed = true

	steps := []string{
		"mcp_openx_mcp_ox_execute_deal_from_prompt_inputs",
		"mcp_indexexchange_ix_execute_deal_from_prompt_inputs",
		"mcp_sendgrid_send_email",
	}
	for i, tool := range steps {
		args := fmt.Sprintf(`{"step":%d}`, i)
		blocked, msg := o.checkCriticalTool(tool, "", args)
		if blocked {
			t.Fatalf("step %d (%s) blocked: %s", i+1, tool, msg)
		}
		o.recordToolResult(tool, args, "ok", true)
		if i < len(steps)-1 && !o.auditConfirmed {
			t.Fatalf("audit consumed prematurely after step %d (%s); commitments: %v",
				i+1, tool, o.committedCriticalActions)
		}
	}
	if o.auditConfirmed {
		t.Fatal("audit should have consumed after final step")
	}
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("expected no unexecuted commitments, got %v", missing)
	}
}

func TestCriticalSuffixCoverage_AcrossSSPs(t *testing.T) {
	cases := []struct {
		name string
		tool string
	}{
		{"OpenX execute", "mcp_openx_mcp_ox_execute_deal_from_prompt_inputs"},
		{"IndexExchange execute", "mcp_indexexchange_ix_execute_deal_from_prompt_inputs"},
		{"PubMatic execute", "mcp_pubmatic_pm_execute_deal_from_prompt_inputs"},
		{"Magnite execute", "mcp_magnite_magnite_execute_deal_from_prompt_inputs"},
		{"OpenX create", "mcp_openx_mcp_ox_create_deal"},
		{"IndexExchange create", "mcp_indexexchange_ix_create_marketplace_deal"},
		{"Xandr create", "mcp_xandr_create_xandr_deal"},
		{"OpenX prepared", "mcp_openx_mcp_ox_create_prepared_deal"},
		{"PubMatic prepared", "mcp_pubmatic_pm_create_prepared_deal"},
		{"PubMatic curated", "mcp_pubmatic_pm_create_curated_deal"},
		{"MediaNet prepared", "mcp_medianet_mn_create_prepared_deal"},
		{"TripleLift create", "mcp_triplelift_tl_create_deal"},
		{"sendgrid send_email", "mcp_sendgrid_send_email"},
	}
	for _, tc := range cases {
		if !isCriticalTool(tc.tool) {
			t.Errorf("%s (%s) should be marked critical but isn't", tc.name, tc.tool)
		}
	}
}

func TestBatchAudit_FailedExecutionRetainsCommitment(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{
		"create_deal: OpenX — D1",
		"create_deal: OpenX — D2",
	})
	o.auditConfirmed = true

	const tool = "mcp_openx_mcp_ox_create_deal"

	o.recordToolResult(tool, `{"d":1}`, "ok", true)
	if got := o.committedCriticalActions["create_deal"]; got != 1 {
		t.Fatalf("after success expected count=1, got %d", got)
	}
	if !o.auditConfirmed {
		t.Fatal("audit should still be valid after one success of two")
	}

	o.recordToolResult(tool, `{"d":2}`, "boom", false)
	if got := o.committedCriticalActions["create_deal"]; got != 1 {
		t.Fatalf("after failure expected count to remain 1, got %d", got)
	}
	if !o.auditConfirmed {
		t.Fatal("audit token was consumed by a failed execution; should remain valid for retry")
	}

	o.recordToolResult(tool, `{"d":2-retry}`, "ok", true)
	if got := o.committedCriticalActions["create_deal"]; got != 0 {
		t.Fatalf("after retry success expected count=0, got %d", got)
	}
	if o.auditConfirmed {
		t.Fatal("audit should have consumed after final commitment discharged")
	}
}

func TestBatchAudit_FallbackToolSatisfiesCommitment(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{
		"execute_deal_from_prompt_inputs: TWC Pollen",
		"execute_deal_from_prompt_inputs: TWC Sinus",
		"execute_deal_from_prompt_inputs: TWC Allergy",
		"send_email: deal_sheet → trader@example.com",
	})
	o.auditConfirmed = true

	o.recordToolResult(
		"mcp_indexexchange_mcp_ix_execute_deal_from_prompt_inputs",
		`{"deal":"pollen"}`,
		"seat resolution failed",
		false,
	)
	o.recordToolResult(
		"mcp_indexexchange_mcp_ix_create_marketplace_deal",
		`{"deal":"pollen-via-create"}`,
		"ok",
		true,
	)
	if got := o.committedCriticalActions["execute_deal_from_prompt_inputs"]; got != 2 {
		t.Fatalf("after fallback success expected execute count=2, got %d", got)
	}

	o.recordToolResult("mcp_indexexchange_mcp_ix_create_marketplace_deal", `{"d":"sinus"}`, "ok", true)
	o.recordToolResult("mcp_indexexchange_mcp_ix_create_marketplace_deal", `{"d":"allergy"}`, "ok", true)
	if got := o.committedCriticalActions["execute_deal_from_prompt_inputs"]; got != 0 {
		t.Fatalf("expected all execute commitments discharged via fallback, got %d remaining", got)
	}

	if got := o.committedCriticalActions["send_email"]; got != 1 {
		t.Fatalf("send_email commitment should still be outstanding, got %d", got)
	}
	o.recordToolResult("mcp_sendgrid_send_email", `{"to":"trader@x"}`, "ok", true)
	if got := o.committedCriticalActions["send_email"]; got != 0 {
		t.Fatalf("send_email commitment should be 0 after send, got %d", got)
	}
}

func TestSubstitution_DirectMatchTakesPriority(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{
		"execute_deal_from_prompt_inputs: high-level",
		"create_marketplace_deal: low-level",
	})
	o.auditConfirmed = true

	o.recordToolResult(
		"mcp_indexexchange_mcp_ix_create_marketplace_deal",
		`{"d":1}`,
		"ok",
		true,
	)
	if got := o.committedCriticalActions["create_marketplace_deal"]; got != 0 {
		t.Fatalf("direct match should discharge create_marketplace_deal to 0, got %d", got)
	}
	if got := o.committedCriticalActions["execute_deal_from_prompt_inputs"]; got != 1 {
		t.Fatalf("execute commitment should NOT be discharged by direct create call, got %d", got)
	}
}

func TestSubstitution_NoArbitrarySubstitution(t *testing.T) {
	if substituteSatisfies("send_email", "create_marketplace_deal") {
		t.Fatal("send_email must never be satisfied by create_marketplace_deal")
	}
	if substituteSatisfies("create_marketplace_deal", "send_email") {
		t.Fatal("create_marketplace_deal must never be satisfied by send_email")
	}
	if substituteSatisfies("generate_presentation", "create_deal") {
		t.Fatal("generate_presentation must never be satisfied by create_deal")
	}
}

func TestLegacyAudit_ConsumesOnFirstCriticalExecution(t *testing.T) {
	o := newOrchStateForTest()
	o.auditConfirmed = true

	const tool = "mcp_openx_mcp_ox_create_deal"
	o.recordToolResult(tool, "{}", "ok", true)
	if o.auditConfirmed {
		t.Fatal("audit should consume on first critical execution when no commitments registered")
	}

	blocked, _ := o.checkCriticalTool(tool, "", "{}")
	if !blocked {
		t.Fatal("second critical call should be blocked after legacy audit consumed")
	}
}

func TestFinishBlockedUntilCommitmentsDischarged(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{"create_deal: A", "create_deal: B"})
	o.auditConfirmed = true
	o.selfAuditRequested = true
	o.selfAuditConfirmedOnce = true

	const tool = "mcp_openx_mcp_ox_create_deal"
	o.recordToolResult(tool, `{"d":1}`, "ok", true)

	allowed, errs := o.checkFinishEnforcement()
	if allowed {
		t.Fatal("finish should be blocked while commitments remain")
	}
	if !anyContains(errs, "create_deal") {
		t.Fatalf("finish-block message should mention unexecuted create_deal, got %v", errs)
	}

	o.recordToolResult(tool, `{"d":2}`, "ok", true)
	allowed, errs = o.checkFinishEnforcement()
	if !allowed {
		t.Fatalf("finish should be allowed after all commitments discharged, got errs: %v", errs)
	}
}

func anyContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

func TestCriticalToolRetryBudget_BlocksAfterMaxAttempts(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{"mcp_openx_mcp_ox_create_prepared_deal: D1"})
	o.auditConfirmed = true

	const tool = "mcp_openx_mcp_ox_create_prepared_deal"
	const argsA = `{"deal":"D1","seg":"bad-segment"}`

	if blocked, msg := o.checkCriticalTool(tool, "", argsA); blocked {
		t.Fatalf("attempt 1 should not be blocked, got: %s", msg)
	}
	o.recordToolResult(tool, argsA, "error: bad segment", false)

	if blocked, msg := o.checkCriticalTool(tool, "", argsA); blocked {
		t.Fatalf("attempt 2 should not be blocked (cap is %d, counter is 1), got: %s",
			maxAttemptsPerCriticalAction, msg)
	}
	o.recordToolResult(tool, argsA, "error: bad segment", false)

	blocked, msg := o.checkCriticalTool(tool, "", argsA)
	if !blocked {
		t.Fatal("attempt 3 with identical args should be blocked once retry budget is exhausted")
	}
	if !strings.Contains(msg, "failed 2 times with identical args") {
		t.Fatalf("expected retry-budget block message naming the attempt count, got: %s", msg)
	}
	if !strings.Contains(msg, "confirm_audit(success=false") {
		t.Fatalf("block message should suggest the abort path, got: %s", msg)
	}

	const argsB = `{"deal":"D1","seg":"different-segment"}`
	if blocked, msg := o.checkCriticalTool(tool, "", argsB); blocked {
		t.Fatalf("a different argsHash should get a fresh retry budget, got blocked: %s", msg)
	}
}

func TestCriticalToolRetryBudget_SuccessClearsCounter(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{
		"mcp_openx_mcp_ox_create_prepared_deal: D1",
		"mcp_openx_mcp_ox_create_prepared_deal: D2",
	})
	o.auditConfirmed = true

	const tool = "mcp_openx_mcp_ox_create_prepared_deal"
	const args = `{"deal":"D1"}`

	o.recordToolResult(tool, args, "transient timeout", false)
	if got := o.criticalToolFailureAttempts[retryBudgetKey(tool, hashString(args))]; got != 1 {
		t.Fatalf("expected counter=1 after one failure, got %d", got)
	}
	o.recordToolResult(tool, args, `{"deal_id":"OX-1"}`, true)
	if got := o.criticalToolFailureAttempts[retryBudgetKey(tool, hashString(args))]; got != 0 {
		t.Fatalf("expected counter cleared after success, got %d", got)
	}
}

func TestRegisterCommittedActions_WarnsOnUnmatchedDeclarations(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := newOrchStateForTest()
	o.registerCommittedActions([]string{
		"Create the OpenX deal and notify the trader",
		"Send some kind of report",
	})

	if len(o.committedCriticalActions) != 0 {
		t.Fatalf("expected zero commitments registered from paraphrased input, got %v",
			o.committedCriticalActions)
	}
	output := buf.String()
	if !strings.Contains(output, "WARNING") || !strings.Contains(output, "NONE matched") {
		t.Fatalf("expected WARNING about unmatched declarations, got log:\n%s", output)
	}
}

func TestRegisterCommittedActions_NoWarningOnEmptyInput(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := newOrchStateForTest()
	o.registerCommittedActions([]string{})

	if strings.Contains(buf.String(), "WARNING") {
		t.Fatalf("empty declaration list must not trigger the warning, got log:\n%s", buf.String())
	}
}

func TestCriticalActionToolNames_PrefersStructuredOverLegacy(t *testing.T) {
	input := confirmAuditInput{
		CriticalActions: []criticalActionStruct{
			{Tool: "mcp_openx_mcp_ox_create_prepared_deal", Identifier: "AdGreetings_PG_25"},
			{Tool: "mcp_sendgrid_send_email", Identifier: "trader@example.com"},
		},
		CriticalActionsBeingUnblocked: []string{"create the OpenX deal and send the report"},
	}
	got := criticalActionToolNames(input)
	want := []string{"mcp_openx_mcp_ox_create_prepared_deal", "mcp_sendgrid_send_email"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tool names, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

func TestCriticalActionToolNames_FallsBackToLegacy(t *testing.T) {
	input := confirmAuditInput{
		CriticalActionsBeingUnblocked: []string{
			"mcp_openx_mcp_ox_create_prepared_deal: AdGreetings_PG_25",
			"mcp_sendgrid_send_email: trader@example.com",
		},
	}
	got := criticalActionToolNames(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries from legacy form, got %d (%v)", len(got), got)
	}
}

func TestCriticalActionToolNames_DropsEmptyTools(t *testing.T) {
	input := confirmAuditInput{
		CriticalActions: []criticalActionStruct{
			{Tool: "mcp_openx_mcp_ox_create_prepared_deal"},
			{Tool: "", Identifier: "no tool here"},
			{Tool: "mcp_sendgrid_send_email"},
		},
	}
	got := criticalActionToolNames(input)
	if len(got) != 2 {
		t.Fatalf("expected 2 non-empty tool names, got %d (%v)", len(got), got)
	}
}

func TestStructuredAuditEndToEnd(t *testing.T) {
	o := newOrchStateForTest()

	input := confirmAuditInput{
		CriticalActions: []criticalActionStruct{
			{Tool: "mcp_openx_mcp_ox_create_prepared_deal", Identifier: "AdGreetings_PG_25"},
			{Tool: "mcp_sendgrid_send_email", Identifier: "trader@example.com"},
		},
	}
	o.registerCommittedActions(criticalActionToolNames(input))

	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("expected 1 create_prepared_deal commitment, got %d", got)
	}
	if got := o.committedCriticalActions["send_email"]; got != 1 {
		t.Fatalf("expected 1 send_email commitment, got %d", got)
	}

	o.auditConfirmed = true
	o.recordToolResult("mcp_openx_mcp_ox_create_prepared_deal", "{}", "ok", true)
	if !o.auditConfirmed {
		t.Fatal("audit should remain valid after create — email commitment outstanding")
	}
	o.recordToolResult("mcp_sendgrid_send_email", "{}", `{"queued":true}`, true)
	if o.auditConfirmed {
		t.Fatal("audit should consume after the final commitment is discharged")
	}
}

func TestSingleDeal_AuditCoversCreateAndEmail(t *testing.T) {
	o := newOrchStateForTest()

	o.registerCommittedActions([]string{
		"mcp_openx_mcp_ox_create_prepared_deal: AdGreetings_PG_25",
		"mcp_sendgrid_send_email: trader@example.com",
	})

	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("expected 1 create_prepared_deal commitment, got %d", got)
	}
	if got := o.committedCriticalActions["send_email"]; got != 1 {
		t.Fatalf("expected 1 send_email commitment, got %d", got)
	}

	o.auditConfirmed = true

	const createTool = "mcp_openx_mcp_ox_create_prepared_deal"
	blocked, msg := o.checkCriticalTool(createTool, "", `{"deal":"AdGreetings_PG_25"}`)
	if blocked {
		t.Fatalf("create_prepared_deal blocked unexpectedly: %s", msg)
	}
	o.recordToolResult(createTool, `{"deal":"AdGreetings_PG_25"}`, `{"deal_id":"OX-bef-V06sGg"}`, true)

	if !o.auditConfirmed {
		t.Fatal("audit token consumed by the create — email step would be blocked.")
	}
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 0 {
		t.Fatalf("create_prepared_deal commitment count after success: want 0, got %d", got)
	}

	const emailTool = "mcp_sendgrid_send_email"
	blocked, msg = o.checkCriticalTool(emailTool, "", `{"to":"trader@example.com"}`)
	if blocked {
		t.Fatalf("send_email blocked unexpectedly: %s", msg)
	}
	o.recordToolResult(emailTool, `{"to":"trader@example.com"}`, `{"queued":true,"message_id":"abc"}`, true)

	if o.auditConfirmed {
		t.Fatal("audit should consume after the final commitment (send_email) is discharged")
	}
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("expected no unexecuted commitments after create+email, got %v", missing)
	}
}

func TestRepeatedCallGuard_BlocksAfterCap(t *testing.T) {
	o := newOrchStateForTest()

	code := `{"code":"html = template.replace('{{x}}', x)"}`
	for i := 0; i < maxConsecutiveIdenticalCalls; i++ {
		if blocked, msg := o.checkRepeatedCall("run_python", code); blocked {
			t.Fatalf("call %d blocked unexpectedly: %s", i+1, msg)
		}
	}

	blocked, msg1 := o.checkRepeatedCall("run_python", code)
	if !blocked {
		t.Fatal("expected loop guard to block call after cap")
	}
	if !strings.Contains(msg1, "LOOP_GUARD") {
		t.Fatalf("guard message missing LOOP_GUARD marker: %s", msg1)
	}
	blocked, msg2 := o.checkRepeatedCall("run_python", code)
	if !blocked {
		t.Fatal("expected loop guard to keep blocking")
	}
	if msg1 == msg2 {
		t.Fatal("consecutive guard messages are identical; they must differ to inject new context tokens")
	}
}

func TestRepeatedCallGuard_ResetsOnChange(t *testing.T) {
	o := newOrchStateForTest()

	code := `{"code":"x = 1"}`
	for i := 0; i < maxConsecutiveIdenticalCalls; i++ {
		if blocked, _ := o.checkRepeatedCall("run_python", code); blocked {
			t.Fatalf("call %d blocked unexpectedly", i+1)
		}
	}

	if blocked, _ := o.checkRepeatedCall("run_python", `{"code":"print(x)"}`); blocked {
		t.Fatal("changed args should reset the guard")
	}
	if blocked, _ := o.checkRepeatedCall("run_python", code); blocked {
		t.Fatal("original args after a reset should not be blocked")
	}

	o2 := newOrchStateForTest()
	sendArgs := `{"to_email":"a@b.com","subject":"s"}`
	for i := 0; i < maxConsecutiveIdenticalCalls; i++ {
		o2.checkRepeatedCall("mcp_sendgrid_send_email", sendArgs)
	}
	o2.checkRepeatedCall("confirm_audit", `{"success":true}`)
	if blocked, _ := o2.checkRepeatedCall("mcp_sendgrid_send_email", sendArgs); blocked {
		t.Fatal("send_email after an interleaved confirm_audit should not be blocked")
	}
}

// ── orchestration sub-tests lifted from cutlass execute_test.go ──

func TestOrchestrationCheckCriticalToolBlocksWithoutAudit(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)

	blocked, msg := orch.checkCriticalTool("send_email", "call-1", `{"to":"test@test.com"}`)
	if !blocked {
		t.Fatal("expected send_email to be blocked without audit")
	}
	if !strings.Contains(msg, "BLOCKED") {
		t.Fatalf("expected BLOCKED message, got: %s", msg)
	}

	blocked, _ = orch.checkCriticalTool("bash", "call-2", `{"command":"ls"}`)
	if blocked {
		t.Fatal("bash should not be blocked by audit gating")
	}
}

func TestOrchestrationCheckCriticalToolAllowsAfterAudit(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)
	orch.auditConfirmed = true

	blocked, _ := orch.checkCriticalTool("send_email", "call-1", `{"to":"test@test.com"}`)
	if blocked {
		t.Fatal("send_email should be allowed after audit confirmation")
	}
}

func TestOrchestrationEmailRateLimit(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)
	orch.auditConfirmed = true
	orch.sendEmailSuccessCount = maxSendEmailCallsPerTask

	blocked, msg := orch.checkCriticalTool("send_email", "call-1", `{}`)
	if !blocked {
		t.Fatal("expected send_email to be rate limited")
	}
	if !strings.Contains(msg, "Safety Limit") {
		t.Fatalf("expected rate limit message, got: %s", msg)
	}
}

func TestOrchestrationEmailDedup(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)
	orch.auditConfirmed = true

	payload := `{"to":"test@test.com","subject":"Hello"}`

	blocked, _ := orch.checkCriticalTool("send_email", "call-1", payload)
	if blocked {
		t.Fatal("first send_email should not be blocked")
	}

	orch.recordToolResult("send_email", payload, `{"status_code":202,"message_id":"test-1"}`, true)

	orch.mu.Lock()
	orch.auditConfirmed = true
	orch.mu.Unlock()

	blocked, msg := orch.checkCriticalTool("send_email", "call-2", payload)
	if !blocked {
		t.Fatal("expected duplicate send_email to be blocked")
	}
	if !strings.Contains(msg, "Duplicate") {
		t.Fatalf("expected duplicate message, got: %s", msg)
	}
}

func TestOrchestrationRecordToolResultTracksCriticalActions(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)

	orch.checkCriticalTool("send_email", "call-1", `{"to":"x"}`)
	if len(orch.pendingCriticalActions) != 1 {
		t.Fatalf("expected 1 pending action, got %d", len(orch.pendingCriticalActions))
	}

	orch.auditConfirmed = true

	orch.recordToolResult("send_email", `{"to":"x"}`, "Email queued successfully. Message ID: ok", true)

	if len(orch.pendingCriticalActions) != 0 {
		t.Fatalf("expected 0 pending actions after success, got %d", len(orch.pendingCriticalActions))
	}
	if len(orch.completedCriticalActions) != 1 {
		t.Fatalf("expected 1 completed action, got %d", len(orch.completedCriticalActions))
	}
}

func TestOrchestrationFinishEnforcement(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)

	canFinish, msgs := orch.checkFinishEnforcement()
	if canFinish {
		t.Fatal("should not finish without audit")
	}
	if len(msgs) == 0 {
		t.Fatal("expected enforcement message")
	}

	canFinish, _ = orch.checkFinishEnforcement()
	if canFinish {
		t.Fatal("should not finish without audit confirmation")
	}

	orch.mu.Lock()
	orch.selfAuditConfirmedOnce = true
	orch.mu.Unlock()

	canFinish, _ = orch.checkFinishEnforcement()
	if !canFinish {
		t.Fatal("should finish after audit confirmation")
	}
}

func TestOrchestrationFinishEnforcementTaskTracker(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)
	orch.selfAuditRequested = true
	orch.selfAuditConfirmedOnce = true
	orch.taskTrackerUsed = true
	orch.latestTaskTracker = taskTrackerSnapshot{Seen: true, Total: 2, Todo: 1, InProgress: 0, Done: 1}

	canFinish, msgs := orch.checkFinishEnforcement()
	if canFinish {
		t.Fatal("should not finish with pending tasks")
	}
	if !strings.Contains(msgs[0], "1 todo") {
		t.Fatalf("expected task tracker message, got: %s", msgs[0])
	}
}

func TestOrchestrationTerminalAuditFailure(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 100)
	orch.selfAuditRequested = true
	orch.selfAuditConfirmedOnce = true
	orch.auditTerminalFailure = true

	canFinish, _ := orch.checkFinishEnforcement()
	if !canFinish {
		t.Fatal("terminal audit failure should allow finish")
	}
}

// ── session log sub-tests lifted from cutlass execute_test.go + session_log_test.go ──

func TestSessionLogAddMessage(t *testing.T) {
	ls := NewLogSession()
	ls.AddMessage("user", "hello", nil, nil)

	if len(ls.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(ls.Messages))
	}
	if ls.Messages[0].Role != "user" {
		t.Fatalf("expected role 'user', got %q", ls.Messages[0].Role)
	}
	if ls.Messages[0].Content != "hello" {
		t.Fatalf("expected content 'hello', got %q", ls.Messages[0].Content)
	}
}

func TestSessionLogCacheHitRate(t *testing.T) {
	ls := NewLogSession()
	ls.PromptTokens = 1000
	ls.CachedTokens = 800

	rate := ls.CumulativeCacheHitRate()
	if rate < 79.0 || rate > 81.0 {
		t.Fatalf("expected ~80%% cache hit rate, got %.1f%%", rate)
	}
}

func TestRedactSecretsInLog(t *testing.T) {
	input := "api_key=sk-or-v1-abc123def456789abc123def456789ab"
	redacted := RedactSecrets(input)
	if strings.Contains(redacted, "abc123") {
		t.Fatalf("secret not redacted: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker: %s", redacted)
	}
}
