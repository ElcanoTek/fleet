// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"strings"
	"testing"
)

// The finish-enforcement nudge for an unexecuted commitment used to name the
// bare suffix and the deprecated legacy field even when the model had used the
// typed critical_actions list ("You declared [send_email] in your audit's
// critical_actions_being_unblocked …"), so neither the model nor a reader of
// the log could see WHICH commitment — tool and record binding — was owed. In
// Reklaim run 6bd0c212 that hid a commitment bound to record "n/a" (#1536).
func TestFinishNudgeNamesTypedCommitmentAndField(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealID: "OX-123"})
	o.selfAuditRequested = true
	o.selfAuditConfirmedOnce = true

	allowed, msgs := o.checkFinishEnforcement()
	if allowed || len(msgs) != 1 {
		t.Fatalf("finish must be refused with one nudge while the typed commitment is outstanding, got allowed=%v msgs=%v", allowed, msgs)
	}
	nudge := msgs[0]
	if !strings.Contains(nudge, typedCreateToolA+" (record OX-123)") {
		t.Errorf("nudge %q must name the outstanding typed commitment with its record binding", nudge)
	}
	if !strings.Contains(nudge, "audit's critical_actions ") || strings.Contains(nudge, criticalActionsBeingUnblockedField) {
		t.Errorf("nudge %q must name the typed field the model used, not the deprecated legacy one", nudge)
	}
	if !strings.Contains(nudge, "confirm_audit(success=false") {
		t.Errorf("nudge %q must still offer the explicit abort", nudge)
	}
}

// A legacy free-text declaration has no tool name or record to show, so its
// nudge keeps the suffix list and names the legacy field it actually used.
func TestFinishNudgeLegacyDeclarationKeepsSuffixList(t *testing.T) {
	o := newOrchStateForTest()
	o.registerCommittedActions([]string{"create_deal: OpenX — AdGreetings_PG_25"})
	o.auditConfirmed = true
	o.selfAuditRequested = true
	o.selfAuditConfirmedOnce = true

	allowed, msgs := o.checkFinishEnforcement()
	if allowed || len(msgs) != 1 {
		t.Fatalf("finish must be refused with one nudge while the legacy commitment is outstanding, got allowed=%v msgs=%v", allowed, msgs)
	}
	nudge := msgs[0]
	if !strings.Contains(nudge, "[create_deal]") || !strings.Contains(nudge, criticalActionsBeingUnblockedField) {
		t.Errorf("legacy nudge %q must list the suffix and name the legacy field", nudge)
	}
}
