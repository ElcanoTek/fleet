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

// resultRecordingStager is a recordingStager that also takes the result of a
// recorded call (ActionResultRecorder).
type resultRecordingStager struct {
	recordingStager
	results []string // "<tool> <call id> <succeeded> <text>"
}

func (s *resultRecordingStager) RecordActionResult(toolName, toolCallID, resultText string, succeeded bool) {
	s.results = append(s.results, strings.Join([]string{toolName, toolCallID, map[bool]string{true: "ok", false: "failed"}[succeeded], resultText}, " "))
}

// A notify-mode tool in critical_tool_result_cards: once the recorded call
// returns, its record is handed the result once (to show the outcome card it
// carries). A call of another tool, or a second result for the same call, is
// not handed over.
func TestNotifyRecordIsHandedItsCallsResult(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes:    []string{"next_campaign_id", "deploy_page"},
		CriticalToolModes:       map[string]string{"next_campaign_id": ApprovalModeNotify, "deploy_page": ApprovalModeNotify},
		CriticalToolResultCards: []string{"next_campaign_id"},
	})
	sink := &resultRecordingStager{}
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sink)

	for _, call := range [][2]string{{"mcp_deal_sheet_next_campaign_id", "call-1"}, {"mcp_pages_deploy_page", "call-2"}} {
		if blocked, msg := o.checkCriticalToolApproval(call[0], call[1], `{"k":"v"}`); blocked {
			t.Fatalf("%s blocked: %s", call[0], msg)
		}
	}
	o.recordToolResult("mcp_pages_deploy_page", `{"k":"v"}`, `{"version":2}`, true)
	o.recordToolResult("mcp_deal_sheet_next_campaign_id", `{"k":"other"}`, `{"campaign_id":"X"}`, true)
	if len(sink.results) != 0 {
		t.Fatalf("handed over %v, want nothing for an undeclared tool or another call's input", sink.results)
	}
	o.recordToolResult("mcp_deal_sheet_next_campaign_id", `{"k":"v"}`, `{"campaign_id":"ELC1"}`, true)
	o.recordToolResult("mcp_deal_sheet_next_campaign_id", `{"k":"v"}`, `{"campaign_id":"ELC2"}`, true)
	if len(sink.results) != 1 || sink.results[0] != `mcp_deal_sheet_next_campaign_id call-1 ok {"campaign_id":"ELC1"}` {
		t.Fatalf("handed over %v, want the one recorded call's result, once", sink.results)
	}
}

// Without critical_tool_result_cards a notify record is never handed a
// result: the record behaves exactly as before.
func TestNotifyRecordWithoutResultCardsIsNotHandedAResult(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes: []string{"next_campaign_id"},
		CriticalToolModes:    map[string]string{"next_campaign_id": ApprovalModeNotify},
	})
	sink := &resultRecordingStager{}
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sink)
	if blocked, _ := o.checkCriticalToolApproval("mcp_deal_sheet_next_campaign_id", "call-1", `{}`); blocked {
		t.Fatal("notify must not block")
	}
	o.recordToolResult("mcp_deal_sheet_next_campaign_id", `{}`, `{"campaign_id":"ELC1"}`, true)
	if len(sink.results) != 0 || len(o.notifyRecordedCalls) != 0 {
		t.Fatalf("results = %v, pending = %v; want none", sink.results, o.notifyRecordedCalls)
	}
}

// Two recorded calls with the same tool and input cannot be told apart by
// the result hook (it carries no call id), so neither is handed a result:
// a card never lands on the wrong record (Codex on #1722).
func TestNotifyRecordWithAnAmbiguousResultIsNotHandedOne(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes:    []string{"next_campaign_id"},
		CriticalToolModes:       map[string]string{"next_campaign_id": ApprovalModeNotify},
		CriticalToolResultCards: []string{"next_campaign_id"},
	})
	sink := &resultRecordingStager{}
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sink)
	for _, id := range []string{"call-1", "call-2"} {
		if blocked, _ := o.checkCriticalToolApproval("mcp_deal_sheet_next_campaign_id", id, `{"k":"v"}`); blocked {
			t.Fatal("notify must not block")
		}
	}
	o.recordToolResult("mcp_deal_sheet_next_campaign_id", `{"k":"v"}`, `{"campaign_id":"ELC2"}`, true)
	o.recordToolResult("mcp_deal_sheet_next_campaign_id", `{"k":"v"}`, `{"campaign_id":"ELC1"}`, true)
	if len(sink.results) != 0 || len(o.notifyRecordedCalls) != 0 {
		t.Fatalf("results = %v, pending = %v; want none handed over and none left", sink.results, o.notifyRecordedCalls)
	}
}
