package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestChatSchedules_LifecycleAndClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	conv, err := s.CreateConversation(ctx, "trader@elcanotek.com", "Deal Pacing Chat", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// 1. Create a recurring cron patrol.
	sched, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID,
		UserEmail:      "trader@elcanotek.com",
		Title:          "2h Deal Pacing Patrol",
		Prompt:         "Check deal pacing across active campaigns and update the summary table.",
		CronExpr:       "0 */2 * * *",
		Timezone:       "UTC",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}
	if sched.Kind != ChatScheduleKindPatrol || sched.Status != ChatScheduleStatusActive {
		t.Fatalf("unexpected schedule: %+v", sched)
	}

	// 2. Creating a second recurring patrol without AllowMultiple updates in place.
	sched2, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID,
		UserEmail:      "trader@elcanotek.com",
		Title:          "4h Deal Pacing Patrol",
		Prompt:         "Check deal pacing and margin every 4 hours.",
		CronExpr:       "0 */4 * * *",
		Timezone:       "America/New_York",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule (upsert): %v", err)
	}
	if sched2.ID != sched.ID {
		t.Errorf("expected in-place update of %s, got new ID %s", sched.ID, sched2.ID)
	}
	if sched2.Title != "4h Deal Pacing Patrol" || sched2.CronExpr != "0 */4 * * *" || sched2.Timezone != "America/New_York" {
		t.Errorf("unexpected updated schedule: %+v", sched2)
	}

	// 3. Pause the recurring patrol so it does not compete for the same
	// conversation's patrol slot when testing the one-shot timer claim below,
	// then create a one-shot timer alongside it.
	paused := ChatScheduleStatusPaused
	pSched, err := s.UpdateChatSchedule(ctx, "trader@elcanotek.com", conv.ID, sched.ID, ChatSchedulePatch{Status: &paused})
	if err != nil {
		t.Fatalf("UpdateChatSchedule (pause): %v", err)
	}
	if pSched.Status != ChatScheduleStatusPaused {
		t.Errorf("status = %q, want paused", pSched.Status)
	}

	timerSched, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID:  conv.ID,
		UserEmail:       "trader@elcanotek.com",
		Title:           "30m Backfill Check",
		Prompt:          "Verify the backfill completed.",
		DurationSeconds: 1800,
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule (timer): %v", err)
	}
	if timerSched.Kind != ChatScheduleKindTimer || timerSched.MaxRuns != 1 {
		t.Fatalf("unexpected timer schedule: %+v", timerSched)
	}

	// 4. ClaimDueChatSchedules when timer is due.
	future := time.Unix(timerSched.NextRunAt+1, 0).UTC()
	claimed, err := s.ClaimDueChatSchedules(ctx, future, 20)
	if err != nil {
		t.Fatalf("ClaimDueChatSchedules: %v", err)
	}
	if len(claimed) == 0 {
		t.Fatalf("expected at least 1 claimed schedule at %v", future)
	}
	var foundTimer bool
	for _, c := range claimed {
		if c.Schedule.ID == timerSched.ID {
			foundTimer = true
			if c.Schedule.Status != ChatScheduleStatusCompleted || c.Schedule.RunCount != 1 {
				t.Errorf("expected one-shot timer to complete after 1 run, got %+v", c.Schedule)
			}
			if !strings.HasPrefix(c.Input.ClientInputID, ChatPatrolInputKeyPrefix) {
				t.Errorf("unexpected client_input_id: %q", c.Input.ClientInputID)
			}
			if !strings.Contains(c.Input.Message, "30m Backfill Check") {
				t.Errorf("unexpected patrol message: %q", c.Input.Message)
			}
		}
	}
	if !foundTimer {
		t.Errorf("timer schedule %s was not in claimed set", timerSched.ID)
	}

	// 5. Cancel the recurring patrol.
	cSched, err := s.CancelChatSchedule(ctx, "trader@elcanotek.com", conv.ID, sched.ID)
	if err != nil {
		t.Fatalf("CancelChatSchedule: %v", err)
	}
	if cSched.Status != ChatScheduleStatusCancelled {
		t.Errorf("status = %q, want cancelled", cSched.Status)
	}

	activeList, err := s.ListChatSchedules(ctx, "trader@elcanotek.com", conv.ID, false)
	if err != nil {
		t.Fatalf("ListChatSchedules: %v", err)
	}
	if len(activeList) != 0 {
		t.Errorf("expected 0 active/paused schedules after completion+cancel, got %d", len(activeList))
	}
}

// TestListUserPatrols is the Patrols hub's index: the user's own live
// recurring patrols across chats, with each chat's title — not timers, not
// cancelled patrols, not another user's, and not patrols in deleted chats.
func TestListUserPatrols(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const me, other = "me@elcanotek.com", "other@elcanotek.com"

	mk := func(user, convTitle, title, cron string) (*Conversation, *ChatSchedule) {
		t.Helper()
		conv, err := s.CreateConversation(ctx, user, convTitle, "victoria", "", false)
		if err != nil {
			t.Fatalf("CreateConversation: %v", err)
		}
		in := ChatScheduleInput{ConversationID: conv.ID, UserEmail: user, Title: title, Prompt: "check", CronExpr: cron, Timezone: "UTC"}
		if cron == "" {
			in.DurationSeconds = 600
		}
		sched, err := s.CreateChatSchedule(ctx, in)
		if err != nil {
			t.Fatalf("CreateChatSchedule: %v", err)
		}
		return conv, sched
	}

	_, active := mk(me, "Deals chat", "Deal sweep", "0 8 * * 1-5")
	pausedConv, paused := mk(me, "Inbox chat", "Inbox triage", "0 */2 * * *")
	st := ChatScheduleStatusPaused
	if _, err := s.UpdateChatSchedule(ctx, me, pausedConv.ID, paused.ID, ChatSchedulePatch{Status: &st}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	mk(me, "Timer chat", "One-shot", "")
	cancelConv, cancelled := mk(me, "Old chat", "Old patrol", "0 9 * * 1")
	if _, err := s.CancelChatSchedule(ctx, me, cancelConv.ID, cancelled.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	gone, _ := mk(me, "Deleted chat", "Gone patrol", "0 9 * * *")
	if err := s.Delete(ctx, me, gone.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	mk(other, "Their chat", "Their patrol", "0 9 * * *")
	// A patrol I made in a teammate's chat (multiplayer): listed, with the
	// owner kept for the HTTP layer's access re-check.
	theirConv, err := s.CreateConversation(ctx, other, "Team chat", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.CreateUser(ctx, me, "test-password-123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := s.SetConversationMultiplayer(ctx, other, theirConv.ID, true, []string{me}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	mine, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: theirConv.ID, UserEmail: me, Title: "Team sweep", Prompt: "check", CronExpr: "0 10 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule (teammate's chat): %v", err)
	}

	got, err := s.ListUserPatrols(ctx, me, 100, 0)
	if err != nil {
		t.Fatalf("ListUserPatrols: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d patrols, want 3 (two active + paused): %+v", len(got), got)
	}
	var team *UserPatrol
	for i := range got {
		if got[i].ID == mine.ID {
			team = &got[i]
		}
	}
	if team == nil || team.ConversationOwner != other || team.ConversationTitle != "Team chat" {
		t.Fatalf("the patrol in a teammate's chat should be listed with its owner, got %+v", team)
	}
	// Active first, so drop it to check the rest of the order.
	rest := make([]UserPatrol, 0, 2)
	for _, p := range got {
		if p.ID != mine.ID {
			rest = append(rest, p)
		}
	}
	got = rest
	if got[0].ID != active.ID || got[0].ConversationTitle != "Deals chat" || got[0].Status != ChatScheduleStatusActive || got[0].ConversationOwner != me {
		t.Errorf("first row should be the active patrol with its chat title, got %+v", got[0])
	}
	if got[1].ID != paused.ID || got[1].ConversationTitle != "Inbox chat" || got[1].Status != ChatScheduleStatusPaused {
		t.Errorf("second row should be the paused patrol, got %+v", got[1])
	}
}

// TestListUserPatrols_UsesItsIndex pins that the per-user patrols query can
// be served by idx_chat_schedules_user_patrols (migration 096): its WHERE
// clause must keep implying the partial index's predicate, or a later edit to
// either would silently fall back to filtering every user's schedules.
// Sequential scans are disabled for the check, since a tiny test table would
// otherwise be scanned regardless.
func TestListUserPatrols_UsesItsIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx,
		`EXPLAIN SELECT cs.id FROM chat_schedules cs
		   JOIN conversations c ON c.id = cs.conversation_id
		  WHERE cs.user_email = $1
		    AND c.deleted_at IS NULL
		    AND cs.cron_expr <> ''
		    AND cs.status IN ('active', 'paused')
		  ORDER BY (cs.status = 'active') DESC, cs.next_run_at ASC, cs.id ASC
		  LIMIT 100 OFFSET 0`, "someone@x.com")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "idx_chat_schedules_user_patrols") {
		t.Errorf("the patrols query does not use its index:\n%s", plan.String())
	}
}

func TestClaimDueChatSchedules_IgnoresSuggestAdvancedModelApproval(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const user = "patroller@elcanotek.com"

	conv, err := s.CreateConversation(ctx, user, "Command Center refresh", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := s.CreateApproval(ctx, conv.ID, user, "suggest_advanced_model", "", `{"reason":"compacted"}`, time.Now().Unix()+86400, ApprovalSeat{}); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	sched, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID,
		UserEmail:      user,
		Title:          "Command Center refresh (2h)",
		Prompt:         "Refresh the dashboard.",
		CronExpr:       "0 */2 * * *",
		Timezone:       "UTC",
	})
	if err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}

	due := time.Unix(sched.NextRunAt+1, 0).UTC()
	claimed, err := s.ClaimDueChatSchedules(ctx, due, 20)
	if err != nil {
		t.Fatalf("ClaimDueChatSchedules: %v", err)
	}
	var found bool
	for _, c := range claimed {
		if c.Schedule.ID == sched.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending suggest_advanced_model card must not block due chat schedule %s", sched.ID)
	}
}
