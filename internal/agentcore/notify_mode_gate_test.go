// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"errors"
	"strings"
	"testing"
)

// recordingStager implements both ApprovalStager and ActionRecorder.
type recordingStager struct {
	stageID   string
	recorded  []string
	undoHints []string
	recordErr error
}

func (s *recordingStager) Stage(_, _, _ string) (string, error)           { return s.stageID, nil }
func (s *recordingStager) StageSuggestion(string) (string, string, error) { return "", "", nil }
func (s *recordingStager) RecordAction(toolName, _, _, undoHint string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.recorded = append(s.recorded, toolName)
	s.undoHints = append(s.undoHints, undoHint)
	return nil
}

// The operator's case: a long analysis ends in a reversible page publish, and
// the 300-second card is raised at an unpredictable moment they are no longer
// watching. Under notify the write happens and the record lands (#1153).
func TestNotifyModeRunsAndRecords(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes:  []string{"deploy_page"},
		CriticalToolModes:     map[string]string{"deploy_page": ApprovalModeNotify},
		CriticalToolUndoHints: map[string]string{"deploy_page": "Undo with rollback_page."},
	})
	sink := &recordingStager{stageID: "appr-1"}
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sink)

	blocked, _ := o.checkCriticalToolApproval("mcp_pages_deploy_page", "call-1", `{"slug":"x"}`)
	if blocked {
		t.Fatal("a notify-mode tool must run instead of blocking on a card")
	}
	if len(sink.recorded) != 1 || sink.recorded[0] != "mcp_pages_deploy_page" {
		t.Fatalf("recorded = %v, want exactly the one call", sink.recorded)
	}
	if sink.undoHints[0] != "Undo with rollback_page." {
		t.Errorf("undo hint = %q, want the bundle's line", sink.undoHints[0])
	}
}

// The whole justification for running without asking is that the user still
// finds out and can undo it. A transport that cannot say so must not skip the
// question — so recording is load-bearing, not best-effort.
func TestNotifyFallsBackToBlockingWhenItCannotRecord(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes: []string{"deploy_page"},
		CriticalToolModes:    map[string]string{"deploy_page": ApprovalModeNotify},
	})

	// A sink that records but fails.
	failing := &recordingStager{stageID: "appr-2", recordErr: errors.New("db down")}
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(failing)
	blocked, msg := o.checkCriticalToolApproval("mcp_pages_deploy_page", "call-1", `{"slug":"x"}`)
	if !blocked || !strings.Contains(msg, "APPROVAL_REQUIRED") {
		t.Fatalf("record failure: blocked=%v msg=%q, want the blocking card", blocked, msg)
	}

	// A sink that cannot record at all (no ActionRecorder).
	plain := sentinelStager{ret: "appr-3"}
	o2 := newOrchestrationState(NewLogSession(), 100)
	o2.setApprovalSink(plain)
	blocked, msg = o2.checkCriticalToolApproval("mcp_pages_deploy_page", "call-1", `{"slug":"x"}`)
	if !blocked || !strings.Contains(msg, "approval_id=appr-3") {
		t.Fatalf("no recorder: blocked=%v msg=%q, want the blocking card", blocked, msg)
	}
}

// Modes are opt-in per suffix: everything a bundle does not name keeps blocking,
// so this cannot quietly widen an existing deployment.
func TestUndeclaredCriticalToolStillBlocks(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes: []string{"deploy_page", "create_deal"},
		CriticalToolModes:    map[string]string{"deploy_page": ApprovalModeNotify},
	})
	sink := &recordingStager{stageID: "appr-4"}
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sink)
	blocked, msg := o.checkCriticalToolApproval("mcp_ssp_create_deal", "call-1", `{}`)
	if !blocked || !strings.Contains(msg, "APPROVAL_REQUIRED") {
		t.Fatalf("undeclared critical tool: blocked=%v msg=%q, want a blocking card", blocked, msg)
	}
	if len(sink.recorded) != 0 {
		t.Errorf("recorded %v, want nothing — an audit-gated write is not a notification", sink.recorded)
	}
}
