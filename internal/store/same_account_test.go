package store

import (
	"context"
	"testing"
	"time"
)

// Two different accounts whose addresses strings.EqualFold calls the same
// (GREEK SMALL LETTER SIGMA and FINAL SIGMA).
const (
	sigmaOwner = "σ@example.com"
	sigmaFinal = "ς@example.com"
)

// patrolRow is a scanned user-patrol row holding only the creator and the
// chat's owner (scanUserPatrol's twelfth and thirteenth columns).
type patrolRow struct{ creator, chatOwner string }

func (r patrolRow) Scan(dest ...any) error {
	*dest[11].(*string) = r.creator
	*dest[12].(*string) = r.chatOwner
	return nil
}

func TestUserPatrolOwnershipKeepsFoldPairAccountsApart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, caller           string
		row                    patrolRow
		wantMine, wantOwnsChat bool
	}{
		{"other account", sigmaFinal, patrolRow{sigmaOwner, sigmaOwner}, false, false},
		{"own patrol", sigmaOwner, patrolRow{sigmaOwner, sigmaOwner}, true, true},
		{"stored with capitals", sigmaOwner, patrolRow{"Σ@Example.com", "Σ@Example.com"}, true, true},
		{"other account, stored with capitals", sigmaFinal, patrolRow{"Σ@Example.com", "Σ@Example.com"}, false, false},
		{"own patrol in another's chat", sigmaFinal, patrolRow{sigmaFinal, sigmaOwner}, true, false},
	} {
		p, err := scanUserPatrol(c.row, c.caller)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if p.Mine != c.wantMine || p.OwnsChat != c.wantOwnsChat {
			t.Errorf("%s: Mine=%v OwnsChat=%v, want %v %v", c.name, p.Mine, p.OwnsChat, c.wantMine, c.wantOwnsChat)
		}
		if !c.wantMine && p.OwnerEmail != c.row.creator {
			t.Errorf("%s: OwnerEmail=%q, want %q", c.name, p.OwnerEmail, c.row.creator)
		}
	}
}

// An Autonomous auto-submit fires only for the chat's owner: another account,
// even one whose address case-folds to the owner's, is cancelled.
func TestUIAutoSubmitSweepFiresOnlyForTheChatOwner(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	seedAccount(t, s, sigmaOwner, sigmaFinal)
	ctx := context.Background()
	for _, email := range []string{sigmaOwner, sigmaFinal} {
		if err := s.AcceptAutonomousPolicy(ctx, email, CurrentAutonomousPolicyVersion); err != nil {
			t.Fatalf("AcceptAutonomousPolicy %s: %v", email, err)
		}
	}
	conv, err := s.CreateConversation(ctx, sigmaOwner, "Fold pair", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.SetApprovalMode(ctx, sigmaOwner, conv.ID, ConversationApprovalAutonomous); err != nil {
		t.Fatalf("SetApprovalMode: %v", err)
	}

	sweep := func(user, callID string) SweptUIAutoSubmit {
		t.Helper()
		nowMs := time.Now().UnixMilli()
		row, err := s.CreateUIAutoSubmit(ctx, UIAutoSubmit{
			ConversationID: conv.ID,
			UserEmail:      user,
			CallID:         callID,
			ActionID:       "submit",
			ValuesJSON:     `{}`,
			DeadlineMs:     nowMs - 100,
			CreatedAtMs:    nowMs - 10_100,
			UpdatedAtMs:    nowMs - 10_100,
		})
		if err != nil {
			t.Fatalf("CreateUIAutoSubmit %s: %v", user, err)
		}
		got, err := s.SweepAndEnqueueDueUIAutoSubmits(ctx, nowMs, 10, 20)
		if err != nil {
			t.Fatalf("SweepAndEnqueueDueUIAutoSubmits: %v", err)
		}
		for _, item := range got {
			if item.Row.ID == row.ID {
				return item
			}
		}
		t.Fatalf("the sweep did not take %s's row", user)
		return SweptUIAutoSubmit{}
	}

	if got := sweep(sigmaFinal, "call_other"); got.Enqueued || got.Row.Status != UIAutoSubmitStatusCancelled {
		t.Errorf("%q's auto-submit in %q's chat: Enqueued=%v status=%q, want cancelled", sigmaFinal, sigmaOwner, got.Enqueued, got.Row.Status)
	}
	if got := sweep(sigmaOwner, "call_owner"); !got.Enqueued {
		t.Errorf("the owner's own auto-submit was not enqueued (status %q)", got.Row.Status)
	}
}
