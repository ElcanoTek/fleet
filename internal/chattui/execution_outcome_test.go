package chattui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestApprovalReplayDoesNotClaimUnknownExecutionSuccess(t *testing.T) {
	for _, tc := range []struct{ name, body, status string }{
		{"running", `{"status":"approved","executing":true}`, ""},
		{"legacy", `{"status":"approved","execution_unknown":true}`, "approved"},
		{"failed", `{"status":"approved","is_err":true,"result_text":"Tool failed"}`, "approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			status, _, err := NewClient(Config{ServerURL: srv.URL}).ResolveApproval(context.Background(), "c", "a", true)
			if err == nil || status != tc.status {
				t.Fatalf("status=%q err=%v", status, err)
			}
		})
	}
}

func TestRunningApprovalSurvivesDeadlineAndReload(t *testing.T) {
	m := newModel(Config{})
	m.finishApproval(approvalResolvedMsg{card: pendingApproval{id: "a", tool: "schedule_task", expiresAt: 1}, err: approvalRunningError{}})
	m.expireApprovals(time.Now())
	if len(m.pending) != 1 || !m.pending[0].executing {
		t.Fatal("running action expired locally")
	}
	if m.autoResolveCard() != nil {
		t.Fatal("running action automatically replayed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"pending_approvals":[],"resolved_approvals":[{"approval_id":"a","tool":"schedule_task","status":"approved","executing":true,"expires_at":1}]}`)
	}))
	defer srv.Close()
	pending, err := NewClient(Config{ServerURL: srv.URL}).loadApprovals(context.Background(), "c")
	if err != nil || len(pending) != 1 || !pending[0].executing {
		t.Fatalf("reload lost running action: %v %v", pending, err)
	}
}

func TestMissingPatternMetadataAndOptionalEdits(t *testing.T) {
	if parsePatternArgs(map[string]any(nil)) != nil {
		t.Fatal("absent metadata became an empty supported map")
	}
	cron := "0 9 * * *"
	a := pendingApproval{patternArgs: map[string]string{"name": "one-time"}, edits: &ScheduleEdits{Cron: &cron}}
	if !policyMatchesCard(ApprovalDecision{Scope: "pattern", Pattern: "cron=0 9 * * *"}, a) {
		t.Fatal("new optional edit was not matched")
	}
}

func TestPatternCommandPreservesLiteralWhitespace(t *testing.T) {
	fields := approvalCommandFields("/approve a pattern prompt=two  spaces*")
	if len(fields) != 4 || fields[3] != "prompt=two  spaces*" {
		t.Fatalf("glob changed: %#v", fields)
	}
}

func TestSessionConsentIsIndependentOfToolFailure(t *testing.T) {
	m := newModel(Config{})
	m.convID = "c"
	m.finishApproval(approvalResolvedMsg{conversation: "c", tool: "schedule_task", approved: true, status: "approved", decision: ApprovalDecision{Approved: true, Scope: "session"}, err: fmt.Errorf("task creation failed")})
	if d, ok := m.matchCardPolicy(pendingApproval{tool: "schedule_task"}); !ok || !d.Approved {
		t.Fatal("recorded session consent lost on execution failure")
	}
}

func TestResumedModelHelpDoesNotClaimWorkspaceDefault(t *testing.T) {
	m := newModel(Config{})
	m.client.AdoptDefaultModel("workspace/default")
	m.convID = "stored-thread"
	m.runSlash("/model")
	got := m.history[len(m.history)-1]
	if strings.Contains(got, "workspace/default") || !strings.Contains(got, "stored conversation model") {
		t.Fatal(got)
	}
}
