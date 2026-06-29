package httpapi

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// recordingSink captures emitted events for assertions.
type recordingSink struct{ events []string }

func (r *recordingSink) Emit(event string, _ any) { r.events = append(r.events, event) }

func intPtr(n int) *int { return &n }

// TestResolveTimeoutSeconds exercises the #225 resolution chain on the stager:
// per-tool manifest override > per-conversation override > global env default >
// hardcoded default.
func TestResolveTimeoutSeconds(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolTimeouts: map[string]int{"send_email": 60},
	})

	cases := []struct {
		name   string
		global int
		conv   *int
		tool   string
		want   int
	}{
		{"per-tool override wins", 300, intPtr(120), "mcp_sendgrid_send_email", 60},
		{"per-conversation override beats global", 300, intPtr(120), "bash", 120},
		{"global default when no conv override", 300, nil, "bash", 300},
		{"hardcoded default when global non-positive", 0, nil, "bash", defaultApprovalTimeoutSeconds},
		{"non-positive conv override ignored", 300, intPtr(0), "bash", 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &approvalStager{globalTimeoutSeconds: tc.global, convTimeoutSeconds: tc.conv}
			if got := a.resolveTimeoutSeconds(tc.tool); got != tc.want {
				t.Errorf("resolveTimeoutSeconds(%q) = %d, want %d", tc.tool, got, tc.want)
			}
		})
	}
}

// TestStage_AutoApproveInTest verifies the FLEET_AUTO_APPROVE_IN_TEST escape
// hatch (#225): Stage returns the pre-approved sentinel and emits a
// tool.auto_resolved event WITHOUT creating an approval row (a nil store would
// panic if it tried), so the gate is bypassed only when explicitly enabled.
func TestStage_AutoApproveInTest(t *testing.T) {
	sink := &recordingSink{}
	a := &approvalStager{
		ctx:               context.Background(),
		conversationID:    "c1",
		userEmail:         "alice@example.com",
		sink:              sink,
		autoApproveInTest: true,
		// store deliberately nil — auto-approve must short-circuit before any
		// store call, so a nil here is the tripwire proving it does.
	}
	got, err := a.Stage("bash", "call_1", `{"command":"ls"}`)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if got != agentcore.PreApprovedSentinel {
		t.Errorf("Stage returned %q, want the pre-approved sentinel", got)
	}
	if len(sink.events) != 1 || sink.events[0] != "tool.auto_resolved" {
		t.Errorf("events = %v, want exactly [tool.auto_resolved]", sink.events)
	}
}

// sweepFakeStore is a minimal chatStore for the expiry-sweep test. It embeds a
// nil *store.Store (promoting every other method) and overrides only the three
// the sweep touches.
type sweepFakeStore struct {
	*store.Store
	expired      []store.Approval
	claimOutcome map[string]bool // approval id -> whether ClaimApproval wins
	claimedIDs   []string
	appendedCnv  []string
}

func (f *sweepFakeStore) ListExpiredApprovals(_ context.Context, _ int64) ([]store.Approval, error) {
	return f.expired, nil
}

func (f *sweepFakeStore) ClaimApproval(_ context.Context, _, approvalID, _, _ string) (bool, error) {
	f.claimedIDs = append(f.claimedIDs, approvalID)
	return f.claimOutcome[approvalID], nil
}

func (f *sweepFakeStore) AppendHistory(_ context.Context, convID string, _ []agent.HistoryEntry) error {
	f.appendedCnv = append(f.appendedCnv, convID)
	return nil
}

// TestSweepExpiredApprovals_ClaimsAndAppends checks that the sweep auto-denies
// each expired approval it can claim, appends a tool_result for the winners
// only, and lets a lost claim (a user resolving in the grace window) pass
// through untouched — user action wins (#225).
func TestSweepExpiredApprovals_ClaimsAndAppends(t *testing.T) {
	fake := &sweepFakeStore{
		expired: []store.Approval{
			{ID: "a1", UserEmail: "u", ConversationID: "c1", ToolName: "bash", ToolCallID: "tc1", ExpiresAt: 1},
			{ID: "a2", UserEmail: "u", ConversationID: "c2", ToolName: "mcp_sendgrid_send_email", ExpiresAt: 1},
		},
		claimOutcome: map[string]bool{"a1": true, "a2": false}, // a2 already resolved by the user
	}
	s := &Server{store: fake}

	n, err := s.SweepExpiredApprovals(context.Background())
	if err != nil {
		t.Fatalf("SweepExpiredApprovals: %v", err)
	}
	if n != 1 {
		t.Errorf("auto-denied count = %d, want 1 (only the claimable row)", n)
	}
	if len(fake.claimedIDs) != 2 {
		t.Errorf("claim attempted on %v, want both a1 and a2", fake.claimedIDs)
	}
	if len(fake.appendedCnv) != 1 || fake.appendedCnv[0] != "c1" {
		t.Errorf("history appended for %v, want exactly [c1] (the won claim)", fake.appendedCnv)
	}
}

// TestSweepExpiredApprovals_StopsOnCancelledContext verifies the loop bails
// before claiming any row once the context is done, so a near-deadline tick
// never leaves a row claimed-rejected without its history breadcrumb (#225).
func TestSweepExpiredApprovals_StopsOnCancelledContext(t *testing.T) {
	fake := &sweepFakeStore{
		expired: []store.Approval{
			{ID: "a1", UserEmail: "u", ConversationID: "c1", ToolName: "bash", ExpiresAt: 1},
		},
		claimOutcome: map[string]bool{"a1": true},
	}
	s := &Server{store: fake}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the sweep loop runs

	n, err := s.SweepExpiredApprovals(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredApprovals: %v", err)
	}
	if n != 0 {
		t.Errorf("auto-denied count = %d, want 0 (loop must bail on a done context)", n)
	}
	if len(fake.claimedIDs) != 0 {
		t.Errorf("claimed %v, want none (no claim attempted under a done context)", fake.claimedIDs)
	}
}
