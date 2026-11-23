package httpapi

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// supersedeBatchStore records every supersede call (tool and kept card ids) and
// hands out a fresh card id per staged call.
type supersedeBatchStore struct {
	chatStore
	n          int
	tools      []string
	keeps      [][]string
	superseded int64
}

func (s *supersedeBatchStore) SupersedePendingApprovals(_ context.Context, _, _, tool string, keep ...string) (int64, error) {
	s.tools = append(s.tools, tool)
	s.keeps = append(s.keeps, slices.Clone(keep))
	return s.superseded, nil
}

func (s *supersedeBatchStore) CreateApproval(_ context.Context, conv, user, tool, call, args string, expiry int64, _ store.ApprovalSeat) (*store.Approval, error) {
	s.n++
	return &store.Approval{ID: fmt.Sprintf("ap-%d", s.n), ConversationID: conv, UserEmail: user, ToolName: tool, ToolCallID: call, ArgsJSON: args, ExpiresAt: expiry, Status: "pending"}, nil
}

func batchPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_rtb_rule"}})
}

// Live 2026-11-23 (Raptive): one turn staged four rule updates, one card per
// deal. Each staging superseded the cards before it, so three were rejected
// while the agent told the trader all four were waiting. Cards a turn stages
// for the same critical tool are a batch: later calls keep them pending.
func TestStage_CriticalBatchInOneTurnKeepsEveryCard(t *testing.T) {
	batchPolicy(t)
	st := &supersedeBatchStore{}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}

	const tool = "mcp_magnite_publisher_mcp_mgpub_update_rtb_rule"
	for i := 1; i <= 4; i++ {
		if _, err := a.StageCritical(tool, fmt.Sprintf("call-%d", i), fmt.Sprintf(`{"deal_id":%d}`, i)); err != nil {
			t.Fatal(err)
		}
	}
	want := [][]string{nil, {"ap-1"}, {"ap-1", "ap-2"}, {"ap-1", "ap-2", "ap-3"}}
	for i, keep := range st.keeps {
		if !slices.Equal(keep, want[i]) {
			t.Fatalf("supersede call %d kept %v, want %v (every card this turn staged)", i+1, keep, want[i])
		}
	}
}

// A new turn's stager has staged nothing yet: a pending card from an earlier
// turn is still superseded (the agent re-staged it), and so is a non-critical
// tool's card in the same turn (preview_email's re-stage is a retry), and so is
// a card staged outside the critical gate (plain Stage: e.g. send_email's own
// gate), even for a tool the policy lists as critical.
func TestStage_EarlierTurnAndNonCriticalCardsAreStillSuperseded(t *testing.T) {
	batchPolicy(t)
	st := &supersedeBatchStore{}
	turn1 := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}
	turn2 := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}

	const tool = "mcp_magnite_publisher_mcp_mgpub_update_rtb_rule"
	if _, err := turn1.StageCritical(tool, "call-1", `{"deal_id":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := turn2.StageCritical(tool, "call-2", `{"deal_id":1}`); err != nil {
		t.Fatal(err)
	}
	if len(st.keeps[1]) != 0 {
		t.Fatalf("a new turn kept %v; an earlier turn's card must still be superseded", st.keeps[1])
	}

	if _, err := turn2.Stage("mcp_pages_deploy_page", "call-3", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := turn2.Stage("mcp_pages_deploy_page", "call-4", `{}`); err != nil {
		t.Fatal(err)
	}
	if got := st.keeps[3]; len(got) != 0 {
		t.Fatalf("a non-critical tool kept %v; its re-stage is a retry and supersedes", got)
	}

	if _, err := turn2.Stage(tool, "call-5", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := turn2.Stage(tool, "call-6", `{}`); err != nil {
		t.Fatal(err)
	}
	if got := st.keeps[5]; len(got) != 0 {
		t.Fatalf("plain Stage kept %v; only the critical gate's cards form a batch", got)
	}
}

// The critical gate stages through StageCritical whenever the stager offers
// it, so a catalog-critical tool (Tool.Critical, not in the bundle's suffix
// list) gets the same batch treatment as a suffix-listed one (Codex P1, #2231).
func TestStagerOffersCriticalStaging(t *testing.T) {
	var stager any = &approvalStager{}
	if _, ok := stager.(agentcore.CriticalStager); !ok {
		t.Fatal("approvalStager must implement agentcore.CriticalStager, or the critical gate stages through plain Stage and batches are superseded")
	}
}

// The stager reports how many older cards a staged call replaced, so the gate
// can tell the model those will not run.
func TestStage_ReportsSupersededCount(t *testing.T) {
	batchPolicy(t)
	st := &supersedeBatchStore{superseded: 2}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}
	if _, err := a.StageCritical("mcp_magnite_publisher_mcp_mgpub_update_rtb_rule", "call-9", `{}`); err != nil {
		t.Fatal(err)
	}
	var reporter agentcore.SupersedeReporter = a
	if got := reporter.SupersededCount("call-9"); got != 2 {
		t.Fatalf("SupersededCount = %d, want 2", got)
	}
	if got := reporter.SupersededCount("call-other"); got != 0 {
		t.Fatalf("SupersededCount for another call = %d, want 0", got)
	}
}
