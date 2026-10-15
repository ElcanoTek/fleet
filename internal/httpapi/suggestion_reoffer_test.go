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
	prior        *store.Approval
	userTurns    int64
	convModel    string
	staged       int
	countedSince int64
	countedCall  string
}

func (s *reofferStore) Get(context.Context, string, string) (*store.Conversation, error) {
	return &store.Conversation{ID: "c", Model: s.convModel}, nil
}
func (s *reofferStore) LatestApprovalByTool(context.Context, string, string) (*store.Approval, error) {
	return s.prior, nil
}
func (s *reofferStore) CountUserMessagesAfterTimestamp(_ context.Context, _ string, since int64) (int64, error) {
	s.countedSince = since
	return s.userTurns, nil
}
func (s *reofferStore) CountUserMessagesAfterResolution(_ context.Context, _, callID string, resolvedAt int64) (int64, error) {
	s.countedSince, s.countedCall = resolvedAt, callID
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

// The cooldown counts the user's messages since they answered the card, not
// since it appeared: a card left pending for a while must not have its
// cooldown already spent when it is answered.
func TestSuggestionCooldownCountsFromResolution(t *testing.T) {
	st := &reofferStore{prior: &store.Approval{ID: "ap", ToolCallID: "call_1", Status: "approved", CreatedAt: 100, ResolvedAt: 900}, convModel: "fast/model"}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c", userEmail: "u", sink: &recordingSink{}}
	if _, err := a.StageHostSuggestion(suggestionSourceSignal, "r"); err != nil {
		t.Fatal(err)
	}
	if st.countedSince != 900 || st.countedCall != "call_1" {
		t.Errorf("counted from %d/%q, want the resolution (900) anchored on its note (call_1)", st.countedSince, st.countedCall)
	}
	// A row resolved before resolved_at was recorded falls back to created_at.
	st.prior = &store.Approval{Status: "rejected", CreatedAt: 100}
	if _, err := a.StageHostSuggestion(suggestionSourceSignal, "r"); err != nil {
		t.Fatal(err)
	}
	if st.countedSince != 100 {
		t.Errorf("legacy row counted since %d, want created_at", st.countedSince)
	}
}

// lockdownStore is a lockdown conversation with no prior suggestion.
type lockdownStore struct{ reofferStore }

func (s *lockdownStore) Get(context.Context, string, string) (*store.Conversation, error) {
	return &store.Conversation{ID: "c", Model: "fast/model", Lockdown: true}, nil
}

// A lockdown chat is never offered an advanced model its allow-list
// excludes, from any source: accepting would pin a slug the next turn
// refuses.
func TestSuggestionRespectsLockdownAllowList(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		st := &lockdownStore{}
		a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c", userEmail: "u", sink: &recordingSink{},
			modelAllowed: func(string) bool { return allowed }}
		staged, err := a.StageHostSuggestion(suggestionSourceSignal, "r")
		if err != nil {
			t.Fatal(err)
		}
		if staged != allowed {
			t.Errorf("allow-list permits advanced = %v: staged = %v", allowed, staged)
		}
		_, msg, _ := a.StageSuggestion("model asked")
		if !allowed && !strings.Contains(msg, "lockdown") {
			t.Errorf("model-requested suggestion message = %q, want the lockdown suppression", msg)
		}
	}

	// LockdownOnly makes a chat created before it was turned on lockdown too.
	st := &reofferStore{convModel: "fast/model"}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c", userEmail: "u", sink: &recordingSink{},
		modelAllowed: func(string) bool { return false }, lockdownOnly: true}
	if staged, err := a.StageHostSuggestion(suggestionSourceSignal, "r"); err != nil || staged {
		t.Errorf("lockdown-only deployment: staged = %v, err = %v; want suppressed", staged, err)
	}
}
