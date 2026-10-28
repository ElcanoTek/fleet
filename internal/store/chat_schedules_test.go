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
