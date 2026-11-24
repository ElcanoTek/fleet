package store

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A waiting approval card does not hold a scheduled patrol run (ADR-0120,
// docs/CHAT-PATROLS.md). These tests pin the delivery, and what the card and
// the queue look like when the two meet.

const patrolCardUser = "patroller@elcanotek.com"

// patrolCardChat is a chat of patrolCardUser's with one two-hourly patrol.
func patrolCardChat(t *testing.T, s *Store) (*Conversation, *ChatSchedule) {
	t.Helper()
	ctx := context.Background()
	seedAccount(t, s, patrolCardUser)
	conv, err := s.CreateConversation(ctx, patrolCardUser, "Daily report", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	sched, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: patrolCardUser, Title: "Patrol", Prompt: "Check the deals.",
		CronExpr: "0 */2 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}
	return conv, sched
}

// waitingCard stages a pending card for user, armed when arm is set, and
// returns it as stored.
func waitingCard(t *testing.T, s *Store, convID, user, tool string, expiresAt int64, arm bool) Approval {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateApproval(ctx, convID, user, tool, "call_"+tool, `{"deal_id":"D1"}`, expiresAt, ApprovalSeat{})
	if err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if arm {
		if ok, err := s.ArmApprovalResume(ctx, user, a.ID, false); err != nil || !ok {
			t.Fatalf("ArmApprovalResume = %t, %v", ok, err)
		}
	}
	return storedCard(t, s, user, a.ID)
}

func storedCard(t *testing.T, s *Store, user, id string) Approval {
	t.Helper()
	a, err := s.GetApproval(context.Background(), user, id)
	if err != nil || a == nil {
		t.Fatalf("GetApproval(%s) = %v, %v", id, a, err)
	}
	return *a
}

// sameWaitingCard fails unless the card is exactly as it was staged: still
// pending, with the same expiry, staging time and resume arming.
func sameWaitingCard(t *testing.T, s *Store, was Approval) {
	t.Helper()
	now := storedCard(t, s, was.UserEmail, was.ID)
	if now.Status != "pending" || now.ExpiresAt != was.ExpiresAt || now.CreatedAt != was.CreatedAt ||
		now.ResumeState != was.ResumeState || now.ResultText != "" || now.ResolvedAt != 0 {
		t.Fatalf("the waiting card changed:\n was %+v\n now %+v", was, now)
	}
}

// fireNextSlot fires the next slot of patrolCardUser's one schedule in the
// chat and returns its delivery.
func fireNextSlot(t *testing.T, s *Store, dsn, convID string) firedChatSchedule {
	t.Helper()
	list, err := s.ListChatSchedules(context.Background(), patrolCardUser, convID, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListChatSchedules = %+v, %v", list, err)
	}
	fired := fireDueChatSchedules(t, s, dsn, time.Unix(list[0].NextRunAt+1, 0).UTC())
	if len(fired) != 1 {
		t.Fatalf("fired = %+v; want one fire", fired)
	}
	return fired[0]
}

// The rule itself: a scheduled fire that meets a waiting card queues its
// turn, and the card is left exactly as it was. The other reasons a slot is
// turned away still hold: a patrol turn in flight, and a full queue.
func TestScheduledFireRunsWithAnApprovalCardWaiting(t *testing.T) {
	t.Parallel()

	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	conv, sched := patrolCardChat(t, s)
	// An hour left on the card: the run neither waits for it nor resets it.
	card := waitingCard(t, s, conv.ID, patrolCardUser, "update_deal", time.Now().Unix()+3600, true)

	first := fireNextSlot(t, s, dsn, conv.ID)
	if first.delivery.Outcome != models.ChatScheduleQueued || first.delivery.InputID == "" {
		t.Fatalf("fire with a card waiting = %+v; want queued", first.delivery)
	}
	turn := inputByID(t, s, conv.ID, first.delivery.InputID)
	if turn.Mode != InputModePatrol || turn.State != InputStateQueued || turn.UserEmail != patrolCardUser {
		t.Fatalf("queued turn = %+v; want a queued patrol turn of the owner's", turn)
	}
	sameWaitingCard(t, s, card)
	if pending, err := s.ListPendingApprovals(ctx, patrolCardUser, conv.ID); err != nil || len(pending) != 1 || pending[0].ID != card.ID {
		t.Fatalf("pending cards = %+v, %v; want the one waiting card, not re-staged", pending, err)
	}

	// The patrol turn is still queued at the next slot: that is still busy.
	second := fireNextSlot(t, s, dsn, conv.ID)
	if second.delivery.Outcome != models.ChatScheduleBusy || second.delivery.Reason != models.ChatScheduleBusyPatrolInFlight {
		t.Fatalf("fire with a patrol turn in flight = %+v; want busy, patrol_in_flight", second.delivery)
	}

	// Once that turn has ended the next slot runs again, the card still waiting.
	if err := s.MarkInputTerminal(ctx, turn.ID, InputStateCompleted); err != nil {
		t.Fatalf("MarkInputTerminal: %v", err)
	}
	third := fireNextSlot(t, s, dsn, conv.ID)
	if third.delivery.Outcome != models.ChatScheduleQueued || third.delivery.InputID == turn.ID {
		t.Fatalf("fire after the turn ended = %+v; want a new queued turn", third.delivery)
	}
	sameWaitingCard(t, s, card)

	runs, err := openSchedOn(t, dsn).ListChatScheduleRuns(ctx, sched.ID, 10)
	if err != nil || len(runs) != 3 {
		t.Fatalf("run history = %+v, %v; want 3 rows", runs, err)
	}
	// Newest first.
	for i, want := range []struct {
		outcome models.ChatScheduleOutcome
		reason  string
		run     int
	}{
		{models.ChatScheduleQueued, "", 2},
		{models.ChatScheduleBusy, models.ChatScheduleBusyPatrolInFlight, 0},
		{models.ChatScheduleQueued, "", 1},
	} {
		if runs[i].Outcome != want.outcome || runs[i].Reason != want.reason || runs[i].RunNumber != want.run {
			t.Fatalf("run history[%d] = %+v; want %+v", i, runs[i], want)
		}
	}
	for _, r := range runs {
		if r.Reason == models.ChatScheduleBusyApprovalPending {
			t.Fatalf("run history holds an approval_pending row: %+v", r)
		}
	}

	// A full queue still turns a scheduled fire away.
	if err := s.MarkInputTerminal(ctx, third.delivery.InputID, InputStateCompleted); err != nil {
		t.Fatalf("MarkInputTerminal: %v", err)
	}
	if _, _, err := s.EnqueueInput(ctx, InputQueueRow{
		ID: "typed-1", ConversationID: conv.ID, UserEmail: patrolCardUser, ClientInputID: "typed-1",
		Message: "a follow-up", Attachments: "[]", Mode: InputModeQueued, State: InputStateQueued,
	}); err != nil {
		t.Fatalf("EnqueueInput: %v", err)
	}
	sd := openSchedOn(t, dsn)
	list, err := s.ListChatSchedules(ctx, patrolCardUser, conv.ID, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListChatSchedules = %+v, %v", list, err)
	}
	at := time.Unix(list[0].NextRunAt+1, 0).UTC()
	fires, err := sd.ClaimDueChatSchedules(ctx, at, time.Minute, 25, nil)
	if err != nil || len(fires) != 1 {
		t.Fatalf("ClaimDueChatSchedules = %+v, %v", fires, err)
	}
	full, err := s.DeliverChatScheduleFire(ctx, fires[0], 1)
	if err != nil || full.Outcome != models.ChatScheduleBusy || full.Reason != models.ChatScheduleBusyQueueFull {
		t.Fatalf("fire with a full queue = %+v, %v; want busy, queue_full", full, err)
	}
	sameWaitingCard(t, s, card)
}

// A one-shot timer follows the same rule: it fires when it is due, not once
// the card is answered. (Held, it retried every 30 seconds, one history row
// each time, until the card was settled.)
func TestScheduledTimerRunsWithAnApprovalCardWaiting(t *testing.T) {
	t.Parallel()

	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	seedAccount(t, s, patrolCardUser)
	conv, err := s.CreateConversation(ctx, patrolCardUser, "Check back", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: patrolCardUser, Prompt: "Check the sync.", DurationSeconds: 1800,
	}); err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}
	card := waitingCard(t, s, conv.ID, patrolCardUser, "update_deal", time.Now().Unix()+86400, false)

	fired := fireNextSlot(t, s, dsn, conv.ID)
	if fired.delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("timer with a card waiting = %+v; want queued", fired.delivery)
	}
	sameWaitingCard(t, s, card)
	if list, err := s.ListChatSchedules(ctx, patrolCardUser, conv.ID, false); err != nil || len(list) != 0 {
		t.Fatalf("live schedules = %+v, %v; want the timer completed", list, err)
	}
}

// The approval mode changes what a patrol turn may do without a card, never
// whether its slot runs: the delivery reads no mode. A card can wait in an
// Auto-approve or Autonomous chat (one staged before the mode was switched,
// or for an action the mode still asks about), and the run goes ahead there
// too, the card untouched.
func TestScheduledFireRunsWithACardWaitingInEveryApprovalMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{ConversationApprovalManual, ConversationApprovalAuto, ConversationApprovalAutonomous} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, dsn := newTestStoreDSN(t)
			ctx := context.Background()
			conv, _ := patrolCardChat(t, s)
			if err := s.SetApprovalMode(ctx, patrolCardUser, conv.ID, mode); err != nil {
				t.Fatalf("SetApprovalMode(%s): %v", mode, err)
			}
			card := waitingCard(t, s, conv.ID, patrolCardUser, "send_email", time.Now().Unix()+86400, true)
			if fired := fireNextSlot(t, s, dsn, conv.ID); fired.delivery.Outcome != models.ChatScheduleQueued {
				t.Fatalf("fire in a %s chat with a card waiting = %+v; want queued", mode, fired.delivery)
			}
			sameWaitingCard(t, s, card)
		})
	}
}

// A multiplayer chat: only the patrol owner's own cards ever held their
// patrol, so a participant's card never did and still does not; and now the
// owner's does not either. Neither card is touched.
func TestScheduledFireLeavesEveryParticipantsCardWaiting(t *testing.T) {
	t.Parallel()

	const mate = "mate@elcanotek.com"
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	conv, _ := patrolCardChat(t, s)
	seedAccount(t, s, mate)
	if _, err := s.SetConversationMultiplayer(ctx, patrolCardUser, conv.ID, true, []string{mate}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	theirs := waitingCard(t, s, conv.ID, mate, "update_deal", time.Now().Unix()+86400, true)

	// The participant's card alone: the patrol runs, as it always did.
	first := fireNextSlot(t, s, dsn, conv.ID)
	if first.delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("fire with a participant's card waiting = %+v; want queued", first.delivery)
	}
	sameWaitingCard(t, s, theirs)
	if err := s.MarkInputTerminal(ctx, first.delivery.InputID, InputStateCompleted); err != nil {
		t.Fatalf("MarkInputTerminal: %v", err)
	}

	// The owner's card too: the patrol still runs.
	mine := waitingCard(t, s, conv.ID, patrolCardUser, "update_deal", time.Now().Unix()+86400, true)
	second := fireNextSlot(t, s, dsn, conv.ID)
	if second.delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("fire with the owner's card waiting = %+v; want queued", second.delivery)
	}
	sameWaitingCard(t, s, theirs)
	sameWaitingCard(t, s, mine)

	// The turn the patrol queued is the patrol owner's, so a card it stages
	// can only replace the owner's own (SupersedePendingApprovals matches the
	// stager's user): the participant's card for the same tool stays.
	if n, err := s.SupersedePendingApprovals(ctx, patrolCardUser, conv.ID, "update_deal"); err != nil || n != 1 {
		t.Fatalf("SupersedePendingApprovals = %d, %v; want only the owner's card replaced", n, err)
	}
	sameWaitingCard(t, s, theirs)
	if got := storedCard(t, s, patrolCardUser, mine.ID); got.Status != "rejected" || got.ResumeState != ApprovalResumeSuperseded {
		t.Fatalf("the owner's replaced card = %+v; want rejected and never resumed", got)
	}
}

// A card approved while the patrol turn is queued, and one approved while it
// runs: the resume turn is queued behind the patrol turn, exactly once, and
// neither turn is lost.
func TestApprovalSettledWhileThePatrolTurnIsInFlight(t *testing.T) {
	t.Parallel()

	for _, running := range []bool{false, true} {
		name := "queued"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, dsn := newTestStoreDSN(t)
			ctx := context.Background()
			conv, _ := patrolCardChat(t, s)
			card := waitingCard(t, s, conv.ID, patrolCardUser, "update_deal", time.Now().Unix()+86400, true)

			fired := fireNextSlot(t, s, dsn, conv.ID)
			if fired.delivery.Outcome != models.ChatScheduleQueued {
				t.Fatalf("fire = %+v; want queued", fired.delivery)
			}
			if running {
				claimed, err := s.ClaimNextQueuedInput(ctx, conv.ID, "turn-patrol")
				if err != nil || claimed == nil || claimed.ID != fired.delivery.InputID {
					t.Fatalf("ClaimNextQueuedInput = %+v, %v; want the patrol turn", claimed, err)
				}
			}

			// The person approves the card; the call runs and its outcome is recorded.
			if ok, err := s.ClaimApproval(ctx, patrolCardUser, card.ID, "approved", ApprovalExecutingSentinel); err != nil || !ok {
				t.Fatalf("ClaimApproval = %t, %v", ok, err)
			}
			if err := s.SetApprovalResult(ctx, patrolCardUser, card.ID, "deal D1 updated", false); err != nil {
				t.Fatalf("SetApprovalResult: %v", err)
			}
			res, err := s.ClaimApprovalResume(ctx, ApprovalResumeRequest{ConversationID: conv.ID, MaxPerHour: 10, MaxPending: 20})
			if err != nil || res.Input == nil || len(res.Claimed) != 1 || res.Claimed[0].Outcome != ApprovalResumeOutcomeApproved {
				t.Fatalf("ClaimApprovalResume = %+v, %v; want one resume input for the approved card", res, err)
			}

			patrol := inputByID(t, s, conv.ID, fired.delivery.InputID)
			resume := inputByID(t, s, conv.ID, res.Input.ID)
			wantPatrolState := InputStateQueued
			if running {
				wantPatrolState = InputStateRunning
			}
			if patrol.State != wantPatrolState || patrol.Mode != InputModePatrol {
				t.Fatalf("patrol turn = %+v; want it still %s", patrol, wantPatrolState)
			}
			if resume.Mode != InputModeResume || resume.State != InputStateQueued || resume.Position <= patrol.Position {
				t.Fatalf("resume turn = %+v; want it queued behind the patrol turn (position %d)", resume, patrol.Position)
			}

			// A second decision (a replayed settlement) starts nothing more.
			again, err := s.ClaimApprovalResume(ctx, ApprovalResumeRequest{ConversationID: conv.ID, MaxPerHour: 10, MaxPending: 20})
			if err != nil || again.Input != nil || len(again.Claimed) != 0 {
				t.Fatalf("second ClaimApprovalResume = %+v, %v; want nothing left to claim", again, err)
			}
			if rows := resumeRows(t, s, conv.ID); len(rows) != 1 {
				t.Fatalf("resume rows = %+v; want exactly one", rows)
			}

			// The queue hands the turns out in order: the patrol turn, then the resume.
			if !running {
				next, err := s.ClaimNextQueuedInput(ctx, conv.ID, "turn-patrol")
				if err != nil || next == nil || next.ID != patrol.ID {
					t.Fatalf("first drained = %+v, %v; want the patrol turn", next, err)
				}
			}
			if err := s.MarkInputTerminal(ctx, patrol.ID, InputStateCompleted); err != nil {
				t.Fatalf("MarkInputTerminal: %v", err)
			}
			next, err := s.ClaimNextQueuedInput(ctx, conv.ID, "turn-resume")
			if err != nil || next == nil || next.ID != resume.ID {
				t.Fatalf("drained after the patrol turn = %+v, %v; want the resume turn", next, err)
			}
			if last, err := s.ClaimNextQueuedInput(ctx, conv.ID, "turn-none"); err != nil || last != nil {
				t.Fatalf("drained after both = %+v, %v; want an empty queue", last, err)
			}
		})
	}
}

// A card whose time runs out while the patrol turn is in flight is denied by
// the sweep as always. It starts no turn of its own in a chat with a patrol
// (the next run picks the work up), and the patrol turn is not touched.
func TestApprovalTimesOutWhileThePatrolTurnRuns(t *testing.T) {
	t.Parallel()

	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	conv, _ := patrolCardChat(t, s)
	card := waitingCard(t, s, conv.ID, patrolCardUser, "update_deal", time.Now().Unix()+86400, true)

	fired := fireNextSlot(t, s, dsn, conv.ID)
	if fired.delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("fire = %+v; want queued", fired.delivery)
	}
	if claimed, err := s.ClaimNextQueuedInput(ctx, conv.ID, "turn-patrol"); err != nil || claimed == nil {
		t.Fatalf("ClaimNextQueuedInput = %+v, %v", claimed, err)
	}

	// The deadline passes mid-turn.
	if _, err := s.db.ExecContext(ctx, `UPDATE approvals SET expires_at = $2 WHERE id = $1`, card.ID, time.Now().Unix()-5); err != nil {
		t.Fatal(err)
	}
	expired, err := s.ListExpiredApprovals(ctx, time.Now().Unix())
	if err != nil || len(expired) != 1 || expired[0].ID != card.ID {
		t.Fatalf("ListExpiredApprovals = %+v, %v; want the card", expired, err)
	}
	if ok, err := s.ClaimExpiredApproval(ctx, patrolCardUser, card.ID, "rejected", "Approval timed out — auto-denied."); err != nil || !ok {
		t.Fatalf("ClaimExpiredApproval = %t, %v", ok, err)
	}
	res, err := s.ClaimApprovalResume(ctx, ApprovalResumeRequest{ConversationID: conv.ID, MaxPerHour: 10, MaxPending: 20})
	if err != nil || res.Input != nil || res.Skipped != ApprovalResumeSkipTimedOut {
		t.Fatalf("ClaimApprovalResume = %+v, %v; want no turn for a timeout in a chat with a patrol", res, err)
	}
	if rows := resumeRows(t, s, conv.ID); len(rows) != 0 {
		t.Fatalf("resume rows = %+v; want none", rows)
	}
	if got := storedCard(t, s, patrolCardUser, card.ID); got.Status != "rejected" || got.ResumeState != ApprovalResumeSkipped {
		t.Fatalf("timed-out card = %+v; want rejected, resume skipped", got)
	}
	if patrol := inputByID(t, s, conv.ID, fired.delivery.InputID); patrol.State != InputStateRunning {
		t.Fatalf("patrol turn = %+v; want it still running", patrol)
	}
}
