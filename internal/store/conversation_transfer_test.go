package store

import (
	"context"
	"errors"
	"testing"
)

// A chat handed to another person arrives as theirs alone: private, on Manual
// approval, with the previous owner's patrols moved and paused, and nothing
// of the previous audience left queued.
func TestTransferConversation(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const from, to, mate = "jeanne@elcanotek.com", "client@reklaim.com", "mate@elcanotek.com"
	for _, email := range []string{from, to, mate} {
		if _, err := s.CreateUser(ctx, email, "correct horse battery staple"); err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
	}
	conv, err := s.CreateConversation(ctx, from, "Opti workbench", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.SetConversationMultiplayer(ctx, from, conv.ID, true, []string{mate}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET pinned = TRUE, labels = '{client}', auto_approve = TRUE, approval_mode = 'auto',
		        share_token = 'tok', shared_at = 1, mcp_accounts = '{"ox":"seat-b"}'
		  WHERE id = $1`, conv.ID); err != nil {
		t.Fatalf("seed owner state: %v", err)
	}
	patrol, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: from, Title: "Morning board",
		Prompt: "Refresh the board.", CronExpr: "30 6 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule(owner): %v", err)
	}
	matePatrol, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: mate, Title: "Mate's check",
		Prompt: "Check pacing.", CronExpr: "0 9 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule(mate): %v", err)
	}
	// The new owner already takes part in the chat and has a patrol of their
	// own in it.
	if _, err := s.UpdateConversationMultiplayerAccess(ctx, from, conv.ID, []string{to}, nil); err != nil {
		t.Fatalf("add the new owner as a participant: %v", err)
	}
	ownPatrol, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: to, Title: "Client's own check",
		Prompt: "Check delivery.", CronExpr: "0 12 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule(new owner): %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_input_queue (id, conversation_id, user_email, message, mode, state, client_input_id, position, created_at, updated_at)
		 VALUES ('q1', $1, $2, 'queued by the old owner', 'queued', 'queued', 'k1', 1, 1, 1)`, conv.ID, from); err != nil {
		t.Fatalf("seed queue: %v", err)
	}

	assertTransferRefusedUnchanged(ctx, t, s, conv.ID, from, to, mate)

	res, err := s.TransferConversation(ctx, from, conv.ID, " Client@Reklaim.com ")
	if err != nil {
		t.Fatalf("TransferConversation: %v", err)
	}
	if res.FromEmail != from || res.ToEmail != to || res.PatrolsMoved != 1 || res.PatrolsPaused != 2 {
		t.Errorf("result = %+v, want 1 patrol moved and 2 paused (the moved one and the new owner's own) from %s to %s", res, from, to)
	}
	if res.Conversation == nil || res.Conversation.UserEmail != to {
		t.Errorf("the result does not carry the transferred conversation: %+v", res.Conversation)
	}
	if old, _ := s.Get(ctx, from, conv.ID); old != nil {
		t.Error("the previous owner still owns the chat")
	}
	if c, _ := s.GetMultiplayerConversation(ctx, from, conv.ID); c != nil {
		t.Error("the previous owner can still open the chat as a participant")
	}
	c, err := s.Get(ctx, to, conv.ID)
	if err != nil || c == nil {
		t.Fatalf("the new owner cannot open the chat: %v", err)
	}
	if c.Title != "Opti workbench" || c.Persona != "victoria" {
		t.Errorf("the chat itself changed: %+v", c)
	}
	if c.Pinned || len(c.Labels) != 0 || c.AutoApprove || c.ApprovalMode != ConversationApprovalManual ||
		c.ShareToken != "" || c.Multiplayer || c.TeamVisible || len(c.MultiplayerParticipants) != 0 || len(c.MCPAccounts) != 0 {
		t.Errorf("the previous owner's arrangement carried over: %+v", c)
	}

	assertTransferredPatrols(ctx, t, s, conv.ID, patrol.ID, ownPatrol.ID, matePatrol.ID, to, mate)
}

// assertTransferRefusedUnchanged: a transfer that is refused changes nothing.
func assertTransferRefusedUnchanged(ctx context.Context, t *testing.T, s *Store, convID, from, to, mate string) {
	t.Helper()
	for name, tc := range map[string]struct {
		owner, target string
		want          error
	}{
		"not the owner":   {mate, to, ErrConversationNotFound},
		"to themselves":   {from, from, ErrTransferTarget},
		"no such account": {from, "nobody@example.com", ErrTransferTarget},
		"empty target":    {from, " ", ErrTransferTarget},
	} {
		if _, err := s.TransferConversation(ctx, tc.owner, convID, tc.target); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if c, _ := s.Get(ctx, from, convID); c == nil || !c.Multiplayer {
		t.Fatalf("a refused transfer changed the chat: %+v", c)
	}
}

// assertTransferredPatrols: the previous owner's patrol is the new owner's,
// paused, and theirs to resume; a participant's own patrol and the queue are
// as the transfer leaves them.
func assertTransferredPatrols(ctx context.Context, t *testing.T, s *Store, convID, patrolID, ownPatrolID, matePatrolID, to, mate string) {
	t.Helper()
	// The owner's patrol is the new owner's, paused; the participant's is
	// left for the caller's reconciliation.
	// The moved patrol and the one the new owner already had: both paused.
	moved, err := s.ListChatSchedules(ctx, to, convID, false)
	if err != nil || len(moved) != 2 {
		t.Fatalf("new owner's schedules = %+v (err %v), want the moved patrol and their own", moved, err)
	}
	for _, sc := range moved {
		if (sc.ID != patrolID && sc.ID != ownPatrolID) || sc.Status != ChatScheduleStatusPaused {
			t.Errorf("new owner's schedule %s is %q, want one of the two, paused", sc.ID, sc.Status)
		}
	}
	if left, _ := s.ListChatSchedules(ctx, mate, convID, false); len(left) != 1 || left[0].ID != matePatrolID || left[0].Status != ChatScheduleStatusActive {
		t.Errorf("the participant's schedule was touched: %+v", left)
	}
	if row := inputByID(t, s, convID, "q1"); row.State != InputStateCancelled {
		t.Errorf("the previous owner's queued input is %q, want cancelled", row.State)
	}

	// The new owner resumes the patrol as their own.
	active := ChatScheduleStatusActive
	if got, err := s.UpdateChatSchedule(ctx, to, convID, patrolID, ChatSchedulePatch{Status: &active}); err != nil || got.Status != ChatScheduleStatusActive || got.UserEmail != to {
		t.Errorf("resume by the new owner = %+v, %v", got, err)
	}
}

func TestTransferConversationRefusals(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const from, to = "owner@x.com", "next@x.com"
	for _, email := range []string{from, to} {
		if _, err := s.CreateUser(ctx, email, "correct horse battery staple"); err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
	}
	archived, _ := s.CreateConversation(ctx, from, "Old", "victoria", "", false)
	if err := s.SetArchived(ctx, from, archived.ID, true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	locked, _ := s.CreateConversation(ctx, from, "Locked", "victoria", "", true)
	for name, id := range map[string]string{"archived": archived.ID, "lockdown": locked.ID} {
		if _, err := s.TransferConversation(ctx, from, id, to); !errors.Is(err, ErrTransferNotAllowed) {
			t.Errorf("%s chat: err = %v, want ErrTransferNotAllowed", name, err)
		}
	}

	// A viewer and a disabled account cannot take a chat.
	open, _ := s.CreateConversation(ctx, from, "Open", "victoria", "", false)
	if _, err := s.db.ExecContext(ctx, `UPDATE core.users SET role = 'viewer' WHERE email = $1`, to); err != nil {
		t.Fatalf("make viewer: %v", err)
	}
	if _, err := s.TransferConversation(ctx, from, open.ID, to); !errors.Is(err, ErrTransferTarget) {
		t.Errorf("viewer target: err = %v, want ErrTransferTarget", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE core.users SET role = 'member', enabled = FALSE WHERE email = $1`, to); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.TransferConversation(ctx, from, open.ID, to); !errors.Is(err, ErrTransferTarget) {
		t.Errorf("disabled target: err = %v, want ErrTransferTarget", err)
	}
}
