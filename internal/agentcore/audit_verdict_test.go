// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import "testing"

// checkFinishEnforcement returns (true, nil) on a terminal audit failure — an
// aborting agent is deliberately ALLOWED to finish rather than trapped in a
// retry loop. Before #1151 that was the end of it: the verdict died inside the
// run and every layer above saw a clean finish.
func TestAuditVerdictSurvivesTheRun(t *testing.T) {
	orch := &orchestrationState{}
	if aborted, summary, executed := orch.auditVerdict(); aborted || summary != "" || executed != 0 {
		t.Fatalf("fresh state = (%v, %q, %d), want (false, \"\", 0)", aborted, summary, executed)
	}

	orch.auditTerminalFailure = true
	orch.auditSummary = "page unchanged; no file-backed update tool available"
	orch.completedCriticalActions = []string{"mcp_pages_update_page_data"}
	aborted, summary, executed := orch.auditVerdict()
	if !aborted {
		t.Error("a terminal audit failure must be visible to the driver")
	}
	if summary != "page unchanged; no file-backed update tool available" {
		t.Errorf("summary = %q, want the agent's own words", summary)
	}
	if executed != 1 {
		t.Errorf("executed = %d, want 1", executed)
	}

	// A nil state is the interactive path, which has no audit gating at all.
	var nilOrch *orchestrationState
	if aborted, _, _ := nilOrch.auditVerdict(); aborted {
		t.Error("a run with no orchestration state must not report an abort")
	}
}

// A later successful confirm_audit supersedes an earlier failed one: the agent
// re-audited and passed, so the run is not an abort and carries no stale
// summary into the task record.
func TestSuccessfulReauditClearsTheAbort(t *testing.T) {
	orch := &orchestrationState{auditTerminalFailure: true, auditSummary: "first attempt failed"}
	// Mirrors the success branch of the confirm_audit tool.
	orch.auditTerminalFailure = false
	orch.auditSummary = ""
	if aborted, summary, _ := orch.auditVerdict(); aborted || summary != "" {
		t.Errorf("after a successful re-audit: (%v, %q), want (false, \"\")", aborted, summary)
	}
}

// withAuditVerdict is applied at every completeRun exit, including the
// structured-output failure path: a run can abort AND fail its output contract,
// and the abort is the more informative of the two.
func TestWithAuditVerdictStampsTheResult(t *testing.T) {
	orch := &orchestrationState{auditTerminalFailure: true, auditSummary: "aborted"}
	res := withAuditVerdict(Result{FinalText: "done"}, orch)
	if !res.AuditAborted || res.AuditSummary != "aborted" {
		t.Errorf("Result = %+v, want the verdict stamped on", res)
	}
	if res.FinalText != "done" {
		t.Error("stamping the verdict must not disturb the rest of the result")
	}
}
