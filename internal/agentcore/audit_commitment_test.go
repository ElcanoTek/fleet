package agentcore

// Tests for typed critical-action commitment binding (#715) and payload-level
// failure detection (#716), lifted+adapted from the v1 engine's hardening.
// Like the rest of this package's tests they use the DSP fixture policy
// installed by TestMain (agent_policy_test.go) — fleet itself ships none of
// those tool names.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

const (
	typedCreateToolA = "mcp_openx_mcp_ox_create_prepared_deal"
	// Same critical suffix (create_prepared_deal), DIFFERENT server — the
	// wrong-seat shape #715 exists to block.
	typedCreateToolB = "mcp_pubmatic_mcp_pm_create_prepared_deal"
)

// registerTyped is a test helper: registers typed critical_actions on o and
// arms the audit token exactly as a successful typed confirm_audit would.
func registerTyped(t *testing.T, o *orchestrationState, actions ...criticalActionStruct) {
	t.Helper()
	if got := o.registerCommittedActionsTyped(actions); got == 0 {
		t.Fatalf("registerCommittedActionsTyped(%v) registered nothing", actions)
	}
	o.auditConfirmed = true
	o.typedAuditActive = true
}

func TestTypedCommitment_WrongServerCannotRideOrDischarge(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA})

	// The wrong-server call must be BLOCKED up front …
	blocked, msg := o.checkCriticalTool(typedCreateToolB, "", `{"deal":"x"}`)
	if !blocked {
		t.Fatalf("same-suffix call on a different server must not ride the audit")
	}
	if !strings.Contains(msg, "matches no outstanding audited commitment") {
		t.Fatalf("expected commitment-binding block message, got: %s", msg)
	}

	// … and even a (hypothetically executed) wrong-server success must not
	// discharge the commitment.
	o.recordToolResult(typedCreateToolB, `{"deal":"x"}`, `{"deal_id":"PM-1"}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("wrong-server success discharged the commitment: outstanding=%d, want 1", got)
	}
	if !o.auditConfirmed {
		t.Fatal("audit token must remain armed for the committed server's call")
	}

	// The committed server's call rides and discharges normally.
	if blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{"deal":"x"}`); blocked {
		t.Fatalf("committed tool unexpectedly blocked: %s", msg)
	}
	o.recordToolResult(typedCreateToolA, `{"deal":"x"}`, `{"deal_id":"OX-1"}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 0 {
		t.Fatalf("committed tool's success did not discharge: outstanding=%d, want 0", got)
	}
	if o.auditConfirmed {
		t.Fatal("audit token should auto-lock once every commitment is discharged")
	}
}

func TestTypedCommitment_SingleDealIDBinding(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealID: "529786"})

	cases := []struct {
		name    string
		args    string
		blocked bool
	}{
		{"different record id", `{"deal_id":"999"}`, true},
		{"no record id at all", `{"note":"missing id"}`, true},
		{"bound id as string", `{"deal_id":"529786"}`, false},
		{"bound id as number", `{"deal_id":529786}`, false},
		{"bound id under sibling key", `{"internal_deal_id":"529786"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, msg := o.checkCriticalTool(typedCreateToolA, "", tc.args)
			if blocked != tc.blocked {
				t.Fatalf("blocked=%v, want %v (msg: %s)", blocked, tc.blocked, msg)
			}
		})
	}

	// A success on the WRONG record discharges nothing; the bound record does.
	o.recordToolResult(typedCreateToolA, `{"deal_id":"999"}`, `{"deal_id":"999"}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("wrong-record success discharged the commitment: outstanding=%d, want 1", got)
	}
	o.recordToolResult(typedCreateToolA, `{"deal_id":"529786"}`, `{"deal_id":"529786"}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 0 {
		t.Fatalf("bound-record success did not discharge: outstanding=%d, want 0", got)
	}
}

func TestTypedCommitment_BatchBindingAndDigest(t *testing.T) {
	const digest = "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"

	newBatchState := func(t *testing.T) *orchestrationState {
		o := newOrchStateForTest()
		registerTyped(t, o, criticalActionStruct{
			Tool:         typedCreateToolA,
			DealIDs:      []string{"1", "2"},
			ValuesDigest: digest,
		})
		return o
	}

	cases := []struct {
		name    string
		args    string
		blocked bool
		wantMsg string
	}{
		{
			name:    "unapproved record in batch",
			args:    fmt.Sprintf(`{"deal_ids":["1","3"],"values_sha256":%q}`, digest),
			blocked: true,
			wantMsg: "not in the",
		},
		{
			name:    "digest mismatch",
			args:    `{"deal_ids":["1","2"],"values_sha256":"deadbeef"}`,
			blocked: true,
			wantMsg: "values_sha256 does not match",
		},
		{
			name:    "missing digest on a digest-bound approval",
			args:    `{"deal_ids":["1","2"]}`,
			blocked: true,
			wantMsg: "values_sha256 does not match",
		},
		{
			name:    "approved records with matching digest",
			args:    fmt.Sprintf(`{"deal_ids":["1","2"],"values_sha256":%q}`, digest),
			blocked: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newBatchState(t)
			blocked, msg := o.checkCriticalTool(typedCreateToolA, "", tc.args)
			if blocked != tc.blocked {
				t.Fatalf("blocked=%v, want %v (msg: %s)", blocked, tc.blocked, msg)
			}
			if tc.wantMsg != "" && !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("block message %q missing %q", msg, tc.wantMsg)
			}
		})
	}

	// Per-record discharge from the batch RESULT: only succeeded records
	// discharge, a failed row keeps its commitment, and an idempotent re-run
	// does not double-discharge.
	o := newBatchState(t)
	args := fmt.Sprintf(`{"deal_ids":["1","2"],"values_sha256":%q}`, digest)
	o.recordToolResult(typedCreateToolA, args,
		`{"results":[{"deal_id":"1","success":true},{"deal_id":"2","success":false}]}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("after partial batch: outstanding=%d, want 1 (only record 1 succeeded)", got)
	}
	if !o.auditConfirmed {
		t.Fatal("audit token must stay armed while record 2's commitment is outstanding")
	}
	// Resume: record 1 reports success again (idempotent skip), record 2 lands.
	o.recordToolResult(typedCreateToolA, args,
		`{"results":[{"deal_id":"1","success":true},{"deal_id":"2","success":true}]}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 0 {
		t.Fatalf("after resumed batch: outstanding=%d, want 0", got)
	}
	if o.auditConfirmed {
		t.Fatal("audit token should auto-lock after the final record discharges")
	}
}

func TestTypedCommitment_UnapprovedBatchEchoDoesNotDischarge(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealIDs: []string{"1", "2"}})

	// A drifted/echoed results[] row for a record the audit never approved
	// must not discharge a DIFFERENT approved record's commitment.
	o.recordToolResult(typedCreateToolA, `{"deal_ids":["1","2"]}`,
		`{"results":[{"deal_id":"3","success":true}]}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 2 {
		t.Fatalf("unapproved echo discharged a commitment: outstanding=%d, want 2", got)
	}
}

func TestTypedAudit_LegacyBatchFailsClosed(t *testing.T) {
	// A legacy (untyped) audit can never approve a server-side batch: it
	// registers no record ids, so any deal_ids call is refused.
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{"create_prepared_deal: batch of three"})
	o.auditConfirmed = true

	blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{"deal_ids":["1","2","3"]}`)
	if !blocked {
		t.Fatal("a deal_ids batch under an untyped audit must fail closed")
	}
	if !strings.Contains(msg, "not in the") {
		t.Fatalf("expected batch-binding block message, got: %s", msg)
	}
}

// confirmAudit invokes the real confirm_audit tool with a fully-populated
// evidence envelope plus the given critical-action declarations, returning the
// tool response.
func confirmAudit(t *testing.T, orch *orchestrationState, typed []criticalActionStruct, legacy []string) fantasy.ToolResponse {
	t.Helper()
	input := confirmAuditInput{
		Success:                       true,
		Reasoning:                     "checked everything",
		ArtifactsChecked:              []string{"workspace/report.csv"},
		WorkflowSectionsChecked:       []string{"build", "verify"},
		CriticalActions:               typed,
		CriticalActionsBeingUnblocked: legacy,
		SendContractChecked:           true,
		AttachmentsChecked:            []string{},
		RemainingRisks:                []string{},
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal confirm_audit input: %v", err)
	}
	tool := buildConfirmAuditTool(orch)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "audit-1", Name: toolNameConfirmAudit, Input: string(raw)})
	if err != nil {
		t.Fatalf("confirm_audit returned transport error: %v", err)
	}
	return resp
}

func TestConfirmAudit_TypedZeroCommitmentsAuthorizesNothing(t *testing.T) {
	cases := []struct {
		name  string
		typed []criticalActionStruct
		// refused: the audit response is an error and the token stays unarmed
		// (entries that named/paraphrased a real critical suffix without the
		// full server-qualified name). Otherwise the audit is ACCEPTED as an
		// explicit no-op — but with zero commitments the typed gate still
		// blocks every critical call. Both shapes authorize nothing.
		refused bool
	}{
		{"bare suffix", []criticalActionStruct{{Tool: "create_prepared_deal"}}, true},
		{"bare email suffix among unknowns", []criticalActionStruct{{Tool: "send_email"}, {Tool: "do_something_else"}}, true},
		{"paraphrase naming a suffix", []criticalActionStruct{{Tool: "please run create_prepared_deal now"}}, true},
		{"explicit no-op declaration", []criticalActionStruct{{Tool: "none"}}, false},
		{"unrecognized paraphrase", []criticalActionStruct{{Tool: "publish the record on the exchange"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrchStateForTest()
			resp := confirmAudit(t, o, tc.typed, nil)
			if tc.refused {
				if !resp.IsError || !strings.Contains(resp.Content, "Audit Rejected") {
					t.Fatalf("malformed typed declaration must be refused, got: %+v", resp)
				}
				if o.auditConfirmed {
					t.Fatal("refused audit must not arm the token")
				}
			} else {
				if resp.IsError {
					t.Fatalf("no-op typed audit should be accepted, got: %s", resp.Content)
				}
				if !o.auditConfirmed || !o.typedAuditActive {
					t.Fatalf("no-op typed audit should grant completion with the typed gate engaged (confirmed=%v typed=%v)",
						o.auditConfirmed, o.typedAuditActive)
				}
				// The task may finish (a no-op run owes no critical work) …
				o.mu.Lock()
				o.selfAuditRequested = true
				o.mu.Unlock()
				if allowed, msgs := o.checkFinishEnforcement(); !allowed {
					t.Fatalf("no-op typed audit should allow finish, got: %v", msgs)
				}
			}
			// … but in BOTH shapes a zero-commitment typed audit authorizes
			// nothing: any critical call stays blocked (fail closed, #715).
			blocked, _ := o.checkCriticalTool(typedCreateToolA, "", `{}`)
			if !blocked {
				t.Fatal("critical call must remain blocked after a zero-commitment typed audit")
			}
		})
	}
}

func TestConfirmAudit_TypedBindsAndFailsClosedAcrossServers(t *testing.T) {
	o := newOrchStateForTest()
	resp := confirmAudit(t, o, []criticalActionStruct{{Tool: typedCreateToolA}}, nil)
	if resp.IsError {
		t.Fatalf("valid typed audit refused: %s", resp.Content)
	}
	if !o.auditConfirmed || !o.typedAuditActive {
		t.Fatalf("typed audit should arm the token and the binding gate (confirmed=%v typed=%v)",
			o.auditConfirmed, o.typedAuditActive)
	}
	if blocked, _ := o.checkCriticalTool(typedCreateToolB, "", `{}`); !blocked {
		t.Fatal("different-server same-suffix call must be blocked under a typed audit")
	}
	if blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{}`); blocked {
		t.Fatalf("committed tool blocked: %s", msg)
	}
}

func TestConfirmAudit_UntypedKeepsLegacySuffixFallback(t *testing.T) {
	o := newOrchStateForTest()
	resp := confirmAudit(t, o, nil, []string{"create_prepared_deal: launch record", "send_email: summary"})
	if resp.IsError {
		t.Fatalf("legacy audit refused: %s", resp.Content)
	}
	if o.typedAuditActive {
		t.Fatal("untyped audit must not arm the typed binding gate")
	}
	// Suffix-scoped: either server variant may discharge (the pre-#715
	// legacy semantics, kept only for untyped audits).
	if blocked, msg := o.checkCriticalTool(typedCreateToolB, "", `{}`); blocked {
		t.Fatalf("legacy suffix commitment should authorize a same-suffix call: %s", msg)
	}
}

func TestMCPReportedFailure(t *testing.T) {
	cases := []struct {
		name   string
		result string
		want   bool
	}{
		{"explicit success false", `{"success": false}`, true},
		{"success false with error", `{"success": false, "error": "HTTP 400"}`, true},
		{"error string only", `{"error": "boom"}`, true},
		{"error object only", `{"error": {"message": "HTTP 400"}}`, true},
		{"success true", `{"success": true}`, false},
		{"success true with error field", `{"success": true, "error": null}`, false},
		{"null error only", `{"error": null}`, false},
		{"empty error string", `{"error": ""}`, false},
		{"no convention fields", `{"deal_id": "OX-1"}`, false},
		{"non-JSON text", "Email queued successfully.", false},
		{"empty", "", false},
		{"array payload", `[{"success": false}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpReportedFailure(tc.result); got != tc.want {
				t.Fatalf("mcpReportedFailure(%q) = %v, want %v", tc.result, got, tc.want)
			}
		})
	}
}

func TestPayloadFailureDoesNotDischargeCommitment(t *testing.T) {
	cases := []struct {
		name   string
		result string
	}{
		{"success false", `{"success": false, "status": 400}`},
		{"top-level error", `{"error": "upstream 400"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrchStateForTest()
			registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA})

			args := `{"deal":"D1"}`
			// Clean transport (succeeded=true) but the PAYLOAD reports failure:
			// the commitment must stay outstanding and the retry budget must
			// count the attempt (#716).
			o.recordToolResult(typedCreateToolA, args, tc.result, true)
			if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
				t.Fatalf("payload-level failure discharged the commitment: outstanding=%d, want 1", got)
			}
			if !o.auditConfirmed {
				t.Fatal("payload-level failure must not consume the audit token")
			}
			if got := o.criticalToolFailureAttempts[retryBudgetKey(typedCreateToolA, hashString(args))]; got != 1 {
				t.Fatalf("payload-level failure must count against the retry budget, got %d attempts", got)
			}
			// Finish stays refused: the committed action has not actually landed.
			o.selfAuditRequested = true
			o.selfAuditConfirmedOnce = true
			if allowed, _ := o.checkFinishEnforcement(); allowed {
				t.Fatal("finish must be refused while the payload-failed commitment is outstanding")
			}
		})
	}
}

func TestReauditSupersedesStaleSameFamilyCommitment(t *testing.T) {
	o := newOrchStateForTest()
	// First envelope: batch over records 1,2 with digest A.
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealIDs: []string{"1", "2"}, ValuesDigest: "aaaa"})
	// Re-audit corrects the digest for the SAME tool + record-set.
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{
		{Tool: typedCreateToolA, DealIDs: []string{"1", "2"}, ValuesDigest: "bbbb"},
	}); got != 2 {
		t.Fatalf("re-audit registered %d units, want 2", got)
	}
	// The stale envelope's units were retired: only the fresh 2 remain.
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 2 {
		t.Fatalf("stale commitment not superseded: outstanding=%d, want 2", got)
	}
	// Discharging the fresh batch fully clears the obligation (no phantom
	// leftover from the superseded envelope wedging finish enforcement).
	o.auditConfirmed = true
	args := `{"deal_ids":["1","2"],"values_sha256":"bbbb"}`
	o.recordToolResult(typedCreateToolA, args,
		`{"results":[{"deal_id":"1","success":true},{"deal_id":"2","success":true}]}`, true)
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("expected no outstanding commitments after the fresh batch, got %v", missing)
	}
}

func TestNormalizeDealID_LargeAndFloatForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string // JSON args carrying deal_id
		want string
	}{
		{"large integer keeps digits", `{"deal_id": 9007199254740993}`, "9007199254740993"},
		{"integral float folds", `{"deal_id": 529786.0}`, "529786"},
		{"plain int", `{"deal_id": 529786}`, "529786"},
		{"string trims", `{"deal_id": " 529786 "}`, "529786"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callDealID(tc.raw); got != tc.want {
				t.Fatalf("callDealID(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// confirmAuditAbort mirrors confirmAudit for the success=false path, with the
// critical-actions declaration deliberately OMITTED — the shape every observed
// field abort used on its first attempt.
func confirmAuditAbort(t *testing.T, orch *orchestrationState, summary string) fantasy.ToolResponse {
	t.Helper()
	input := confirmAuditInput{
		Success:                 false,
		Reasoning:               "cannot complete safely",
		ArtifactsChecked:        []string{"workspace/report.csv"},
		WorkflowSectionsChecked: []string{"build", "verify"},
		SendContractChecked:     true,
		AttachmentsChecked:      []string{},
		RemainingRisks:          []string{},
		UserVisibleSummary:      summary,
	}
	tool := buildConfirmAuditTool(orch)
	raw, _ := json.Marshal(input)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "abort", Name: toolNameConfirmAudit, Input: string(raw)})
	if err != nil {
		t.Fatalf("confirm_audit(success=false) returned error: %v", err)
	}
	return resp
}

// TestConfirmAudit_AbortDoesNotRequireCriticalActions: an abort unlocks
// nothing, so it must not be rejected for omitting the unlock list. Before this
// every abort in the field cost a wasted "Audit Rejected" round trip.
func TestConfirmAudit_AbortDoesNotRequireCriticalActions(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA})
	resp := confirmAuditAbort(t, o, "source failed validation; nothing written")
	if resp.IsError || !strings.Contains(resp.Content, "Audit Failed Terminally") {
		t.Fatalf("abort with outstanding work must land as terminal failure, got: %+v", resp)
	}
	aborted, summary, _ := o.auditVerdict()
	if !aborted || summary != "source failed validation; nothing written" {
		t.Fatalf("verdict aborted=%v summary=%q", aborted, summary)
	}
	// A success=true audit still needs the declaration.
	o2 := newOrchStateForTest()
	if resp := confirmAudit(t, o2, nil, nil); !resp.IsError || !strings.Contains(resp.Content, "requires critical_actions") {
		t.Fatalf("passing audit without a declaration must still be rejected, got: %+v", resp)
	}
}

// TestReauditSupersedesStaleUnboundSameToolCommitment pins the phantom-
// obligation fix for UNBOUND commitments: declare a write, watch it fail,
// re-audit the same tool, retry successfully → nothing outstanding, finish
// allowed. Before the fix the second declaration stacked on the first and the
// run could only exit through an abort that recorded a live page as an error.
func TestReauditSupersedesStaleUnboundSameToolCommitment(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA})
	// The attempt runs (authorized) and reports a payload-level failure.
	if blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{"expected_version":"537"}`); blocked {
		t.Fatalf("declared call must be authorized, got blocked: %s", msg)
	}
	o.recordToolResult(typedCreateToolA, `{"expected_version":"537"}`, `{"ok":false,"error":"stale_version"}`, true)
	if missing := o.unexecutedCommitments(); len(missing) != 1 {
		t.Fatalf("failed attempt must leave the commitment outstanding, got %v", missing)
	}
	// Re-audit of the SAME unbound tool supersedes instead of stacking.
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA}}); got != 1 {
		t.Fatalf("re-audit registered %d units, want 1", got)
	}
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("stale unbound commitment not superseded: outstanding=%d, want 1", got)
	}
	// One successful retry clears everything.
	o.recordToolResult(typedCreateToolA, `{"expected_version":"546"}`, `{"ok":true,"version":{"id":"549"}}`, true)
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("expected no outstanding commitments after the retry, got %v", missing)
	}
	o.mu.Lock()
	o.selfAuditRequested, o.selfAuditConfirmedOnce = true, true
	o.mu.Unlock()
	if allowed, msgs := o.checkFinishEnforcement(); !allowed {
		t.Fatalf("finish must be allowed once the retry discharged the obligation, got %v", msgs)
	}
}

// TestReauditDoesNotRetireOtherShapesOrOtherTools: the unbound supersede is
// scoped to the exact same full tool and the same binding shape.
func TestReauditDoesNotRetireOtherShapesOrOtherTools(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o,
		criticalActionStruct{Tool: typedCreateToolA},               // unbound A
		criticalActionStruct{Tool: typedCreateToolA, DealID: "77"}, // record-bound A
		criticalActionStruct{Tool: typedCreateToolB},               // unbound B (other server)
	)
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA}}); got != 1 {
		t.Fatalf("re-audit registered %d units, want 1", got)
	}
	// Retired: the unbound A. Kept: the record-bound A and the unbound B.
	// Suffix count = 1 (fresh unbound A) + 1 (bound A) + 1 (B) = 3.
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 3 {
		t.Fatalf("outstanding=%d, want 3 (only the same-shape same-tool commitment is retired)", got)
	}
	outstanding := 0
	for _, c := range o.typedCommitments {
		if c.remaining > 0 {
			outstanding++
		}
	}
	if outstanding != 3 {
		t.Fatalf("typed outstanding=%d, want 3", outstanding)
	}
}

// TestConfirmAudit_AbortAfterAllDeclaredExecutedIsRefused: once every declared
// critical action has executed, an abort is refused (not a terminal failure)
// and finish stays allowed — the run's status must reflect the page that IS
// live, not the bookkeeping the model could not close.
func TestConfirmAudit_AbortAfterAllDeclaredExecutedIsRefused(t *testing.T) {
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: typedCreateToolA}}, nil); resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{}`); blocked {
		t.Fatalf("declared call blocked: %s", msg)
	}
	o.recordToolResult(typedCreateToolA, `{}`, `{"ok":true}`, true)

	resp := confirmAuditAbort(t, o, "aborting the outstanding commitment to avoid a duplicate write")
	if !resp.IsError || !strings.Contains(resp.Content, "Audit Abort Refused") {
		t.Fatalf("abort after completed work must be refused, got: %+v", resp)
	}
	if aborted, _, _ := o.auditVerdict(); aborted {
		t.Fatal("refused abort must not flag the run as a terminal audit failure")
	}
	if allowed, msgs := o.checkFinishEnforcement(); !allowed {
		t.Fatalf("finish must be allowed after the refused abort, got %v", msgs)
	}
	// With work still outstanding the abort remains a real terminal failure.
	o2 := newOrchStateForTest()
	registerTyped(t, o2, criticalActionStruct{Tool: typedCreateToolA})
	if resp := confirmAuditAbort(t, o2, "blocked"); resp.IsError || !strings.Contains(resp.Content, "Audit Failed Terminally") {
		t.Fatalf("abort with outstanding work must still fail terminally, got: %+v", resp)
	}
}

// Field case (Energizer daily, 2026-08-25): the audit declared the inline Pages
// write, the payload had gone by reference, the upload variant was BLOCKED, the
// model aborted (nothing had run) and re-audited the upload tool, which then
// published the page — yet finish enforcement kept demanding the inline
// declaration and forced a second abort, landing a live page as status: error.
// An abort must retire what it abandons so the re-audit's execution is what
// the run is judged on.
func TestConfirmAudit_AbortRetiresUnexecutedCommitmentsSoReauditCanSwitchTool(t *testing.T) {
	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, []criticalActionStruct{{Tool: typedCreateToolA}}, nil); resp.IsError {
		t.Fatalf("first audit should pass: %s", resp.Content)
	}
	// The call the model actually needs is bound to a different tool → blocked.
	if blocked, _ := o.checkCriticalTool(typedCreateToolB, "", `{}`); !blocked {
		t.Fatal("undeclared tool must be blocked")
	}

	resp := confirmAuditAbort(t, o, "declared the wrong tool variant; nothing executed yet")
	if resp.IsError || !strings.Contains(resp.Content, "Audit Failed Terminally") {
		t.Fatalf("abort with outstanding work is a terminal failure at this point, got: %+v", resp)
	}
	if !strings.Contains(resp.Content, "Retired declared-but-unexecuted") || !strings.Contains(resp.Content, typedCreateToolA) {
		t.Fatalf("abort must report what it retired, got: %s", resp.Content)
	}
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("abort left commitments on the ledger: %v", got)
	}
	if aborted, _, _ := o.auditVerdict(); !aborted {
		t.Fatal("abort with nothing executed must flag the run — until a later audit redeems it")
	}

	// Re-audit declaring the right tool, execute it, finish cleanly.
	resp = confirmAudit(t, o, []criticalActionStruct{{Tool: typedCreateToolB}}, nil)
	if resp.IsError {
		t.Fatalf("re-audit should pass: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "Finish now") || !strings.Contains(resp.Content, "Declared and not yet executed") || !strings.Contains(resp.Content, typedCreateToolB) {
		t.Fatalf("re-audit trailer must name the outstanding declaration, not say finish: %s", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(typedCreateToolB, "", `{}`); blocked {
		t.Fatalf("declared tool blocked after re-audit: %s", msg)
	}
	o.recordToolResult(typedCreateToolB, `{}`, `{"ok":true,"version":{"id":"572"}}`, true)
	if aborted, _, _ := o.auditVerdict(); aborted {
		t.Fatal("a successful re-audit + execution must redeem the earlier abort")
	}
	if allowed, msgs := o.checkFinishEnforcement(); !allowed {
		t.Fatalf("finish must be allowed with nothing outstanding, got %v", msgs)
	}
	if got := o.unexecutedCommitments(); len(got) != 0 {
		t.Fatalf("stale declaration resurfaced: %v", got)
	}
	// And a needless second abort now hits the completed-work refusal.
	if resp := confirmAuditAbort(t, o, "aborting the retired inline declaration"); !resp.IsError || !strings.Contains(resp.Content, "Audit Abort Refused") {
		t.Fatalf("abort after the work executed must be refused, got: %+v", resp)
	}
}

// The success trailer must describe the ledger: a fresh declaration is
// outstanding, so the response must say so rather than "Finish now".
func TestConfirmAudit_ConfirmTrailerNamesOutstandingDeclarations(t *testing.T) {
	o := newOrchStateForTest()
	resp := confirmAudit(t, o, []criticalActionStruct{{Tool: typedCreateToolA}}, nil)
	if resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "Finish now") {
		t.Fatalf("trailer told the model to finish with a declaration outstanding: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "Declared and not yet executed") || !strings.Contains(resp.Content, typedCreateToolA) {
		t.Fatalf("trailer must name the outstanding declaration: %s", resp.Content)
	}
	o.recordToolResult(typedCreateToolA, `{}`, `{"ok":true}`, true)
	// A repeat audit with the same fingerprint short-circuits; a materially new
	// one with nothing outstanding may say finish.
	resp = confirmAudit(t, o, []criticalActionStruct{{Tool: "none"}}, nil)
	if resp.IsError {
		t.Fatalf("follow-up audit should pass: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "All 1 critical actions executed. Finish now.") {
		t.Fatalf("trailer with nothing outstanding should count the executed call and say finish: %s", resp.Content)
	}
}
