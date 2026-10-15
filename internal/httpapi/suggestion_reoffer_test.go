package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// reofferStore is frozenSuggestionStore with a prior suggestion and a count of
// user messages since it.
type reofferStore struct {
	frozenSuggestionStore
	prior     *store.Approval
	userTurns int64
	convModel string
	staged    int
}

func (s *reofferStore) Get(context.Context, string, string) (*store.Conversation, error) {
	return &store.Conversation{ID: "c", Model: s.convModel}, nil
}
func (s *reofferStore) LatestApprovalByTool(context.Context, string, string) (*store.Approval, error) {
	return s.prior, nil
}
func (s *reofferStore) CountUserMessagesAfterTimestamp(context.Context, string, int64) (int64, error) {
	return s.userTurns, nil
}
func (s *reofferStore) CreateApproval(ctx context.Context, conv, user, tool, call, args string, expiry int64, seat store.ApprovalSeat) (*store.Approval, error) {
	s.staged++
	return s.frozenSuggestionStore.CreateApproval(ctx, conv, user, tool, call, args, expiry, seat)
}

// An accepted suggestion no longer suppresses a conversation for good: once
// the user has moved back off the advanced model and the cooldown has passed,
// they can be offered it again. While still on it, or inside the cooldown,
// they are not.
func TestSuggestionReofferedAfterSwitchingBack(t *testing.T) {
	adv := agentcore.CurrentAdvancedModel()
	cases := []struct {
		name      string
		status    string
		convModel string
		turns     int64
		want      bool
	}{
		{"accepted, still on advanced", "approved", adv, 10, false},
		{"accepted, switched back, inside cooldown", "approved", "fast/model", int64(agentcore.SuggestAdvancedCooldownTurns) - 1, false},
		{"accepted, switched back, cooldown passed", "approved", "fast/model", int64(agentcore.SuggestAdvancedCooldownTurns), true},
		{"dismissed, cooldown passed", "rejected", "fast/model", int64(agentcore.SuggestAdvancedCooldownTurns), true},
		{"pending", "pending", "fast/model", 10, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &reofferStore{prior: &store.Approval{Status: c.status}, userTurns: c.turns, convModel: c.convModel}
			a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c", userEmail: "u", sink: &recordingSink{}}
			staged, err := a.StageHostSuggestion(suggestionSourceSignal, "3 tool calls in a row failed")
			if err != nil {
				t.Fatal(err)
			}
			if staged != c.want || (st.staged == 1) != c.want {
				t.Fatalf("staged = %v (%d cards), want %v", staged, st.staged, c.want)
			}
			if c.want && !strings.Contains(st.approval.ArgsJSON, `"source":"signal"`) {
				t.Errorf("args = %s, want the source recorded", st.approval.ArgsJSON)
			}
		})
	}
}
