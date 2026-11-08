package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// openSchedOn opens the scheduler's database on the chat test database dsn
// (its sched schema), as `fleet serve` does on one DATABASE_URL.
func openSchedOn(t *testing.T, dsn string) *scheddb.Database {
	t.Helper()
	d := scheddb.New()
	if err := d.Init(config.WithSchema(dsn, config.SchedSchema), scheddb.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1, ConnectTimeout: 10 * time.Second}); err != nil {
		t.Fatalf("open sched: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

type firedChatSchedule struct {
	fire     models.ChatScheduleFire
	delivery models.ChatScheduleDelivery
}

// fireDueChatSchedules runs one pass of the scheduler's chat schedule cycle at
// `at` — claim (lease) → s.DeliverChatScheduleFire → settle — the way
// internal/sched/scheduler does with the chat server as deliverer.
func fireDueChatSchedules(t *testing.T, s *Store, dsn string, at time.Time) []firedChatSchedule {
	t.Helper()
	ctx := context.Background()
	sd := openSchedOn(t, dsn)
	fires, err := sd.ClaimDueChatSchedules(ctx, at, time.Minute, 25, nil)
	if err != nil {
		t.Fatalf("ClaimDueChatSchedules: %v", err)
	}
	out := make([]firedChatSchedule, 0, len(fires))
	for _, f := range fires {
		d, err := s.DeliverChatScheduleFire(ctx, f, 20)
		if err != nil {
			t.Fatalf("DeliverChatScheduleFire(%s): %v", f.ScheduleID, err)
		}
		if ok, err := sd.SettleChatScheduleFire(ctx, f, d, at); err != nil || !ok {
			t.Fatalf("SettleChatScheduleFire(%s) = %v, %v", f.ScheduleID, ok, err)
		}
		out = append(out, firedChatSchedule{fire: f, delivery: d})
	}
	return out
}

// inputByID reads one input-queue row.
func inputByID(t *testing.T, s *Store, convID, id string) InputQueueRow {
	t.Helper()
	row, err := scanInputRow(s.db.QueryRowContext(context.Background(),
		inputQueueSelect+` WHERE conversation_id = $1 AND id = $2`, convID, id))
	if err != nil {
		t.Fatalf("input %s: %v", id, err)
	}
	return row
}

func TestChatSchedules_LifecycleAndClaim(t *testing.T) {
	t.Parallel()

	s, dsn := newTestStoreDSN(t)
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

	// 4. The scheduler fires the timer when it is due.
	future := time.Unix(timerSched.NextRunAt+1, 0).UTC()
	fired := fireDueChatSchedules(t, s, dsn, future)
	if len(fired) == 0 {
		t.Fatalf("expected at least 1 fired schedule at %v", future)
	}
	var foundTimer bool
	for _, f := range fired {
		if f.fire.ScheduleID != timerSched.ID {
			continue
		}
		foundTimer = true
		if f.delivery.Outcome != models.ChatScheduleQueued {
			t.Fatalf("timer delivery = %+v, want queued", f.delivery)
		}
		row := inputByID(t, s, conv.ID, f.delivery.InputID)
		if !strings.HasPrefix(row.ClientInputID, ChatPatrolInputKeyPrefix) {
			t.Errorf("unexpected client_input_id: %q", row.ClientInputID)
		}
		if !strings.Contains(row.Message, "30m Backfill Check") {
			t.Errorf("unexpected patrol message: %q", row.Message)
		}
	}
	if !foundTimer {
		t.Errorf("timer schedule %s was not fired", timerSched.ID)
	}
	all, err := s.ListChatSchedules(ctx, "trader@elcanotek.com", conv.ID, true)
	if err != nil {
		t.Fatalf("ListChatSchedules: %v", err)
	}
	for _, sc := range all {
		if sc.ID == timerSched.ID && (sc.Status != ChatScheduleStatusCompleted || sc.RunCount != 1 || sc.LastRunAt == nil) {
			t.Errorf("expected one-shot timer to complete after 1 run, got %+v", sc)
		}
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
	t.Parallel()

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
	t.Parallel()

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
		`EXPLAIN SELECT cs.id FROM `+chatSchedulesTable+` cs
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
	t.Parallel()

	s, dsn := newTestStoreDSN(t)
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
	var found bool
	for _, f := range fireDueChatSchedules(t, s, dsn, due) {
		if f.fire.ScheduleID == sched.ID && f.delivery.Outcome == models.ChatScheduleQueued {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending suggest_advanced_model card must not block due chat schedule %s", sched.ID)
	}
	pending, err := s.ListPendingApprovals(ctx, user, conv.ID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected pending suggest_advanced_model card to be superseded when patrol is claimed, got %+v", pending)
	}
}

// A pause or cancel that lands between a fire's delivery and its settle
// revokes the fire: the queued turn is cancelled, the settle finds no lease
// and records nothing, so the run is not counted and a paused one-shot timer
// stays paused (not completed).
func TestChatSchedulePauseBetweenDeliveryAndSettle(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	const user = "pauser@elcanotek.com"
	sd := openSchedOn(t, dsn)

	for _, tc := range []struct {
		name   string
		in     ChatScheduleInput
		status string
	}{
		{"paused timer", ChatScheduleInput{Prompt: "Ping.", DurationSeconds: 600}, ChatScheduleStatusPaused},
		{"cancelled patrol", ChatScheduleInput{Prompt: "Check.", CronExpr: "*/10 * * * *"}, ChatScheduleStatusCancelled},
	} {
		conv, err := s.CreateConversation(ctx, user, tc.name, "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		tc.in.ConversationID, tc.in.UserEmail = conv.ID, user
		sc, err := s.CreateChatSchedule(ctx, tc.in)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Unix(sc.NextRunAt+1, 0)
		fires, err := sd.ClaimDueChatSchedules(ctx, at, time.Minute, 25, nil)
		if err != nil || len(fires) != 1 {
			t.Fatalf("%s: claim = %+v, %v", tc.name, fires, err)
		}
		d, err := s.DeliverChatScheduleFire(ctx, fires[0], 20)
		if err != nil || d.Outcome != models.ChatScheduleQueued {
			t.Fatalf("%s: deliver = %+v, %v", tc.name, d, err)
		}
		st := tc.status
		if _, err := s.UpdateChatSchedule(ctx, user, conv.ID, sc.ID, ChatSchedulePatch{Status: &st}); err != nil {
			t.Fatalf("%s: %s: %v", tc.name, st, err)
		}
		if ok, err := sd.SettleChatScheduleFire(ctx, fires[0], d, at); err != nil || ok {
			t.Fatalf("%s: settle after %s = %v, %v; want the fire revoked", tc.name, st, ok, err)
		}
		if row := inputByID(t, s, conv.ID, d.InputID); row.State != InputStateCancelled {
			t.Errorf("%s: delivered turn state = %q, want cancelled", tc.name, row.State)
		}
		all, err := s.ListChatSchedules(ctx, user, conv.ID, true)
		if err != nil || len(all) != 1 || all[0].Status != st || all[0].RunCount != 0 {
			t.Fatalf("%s: schedule = %+v, %v; want %s with no run counted", tc.name, all, err, st)
		}
		if runs, _ := sd.ListChatScheduleRuns(ctx, sc.ID, 10); len(runs) != 0 {
			t.Errorf("%s: run history = %+v, want none", tc.name, runs)
		}
	}
}

// TestChatScheduleDeliveryTrustsOnlyPatrolMode: patrol provenance is the
// server-owned mode, not the caller's client_input_id. A user input that
// borrows a chat-patrol: key neither makes a due fire busy, nor is cancelled
// when the schedule is paused.
func TestChatScheduleDeliveryTrustsOnlyPatrolMode(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	const user = "trust@elcanotek.com"
	conv, err := s.CreateConversation(ctx, user, "c", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: user, Prompt: "P", CronExpr: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	forged, _, err := s.EnqueueInput(ctx, InputQueueRow{
		ID: "forged", ConversationID: conv.ID, UserEmail: user,
		ClientInputID: ChatPatrolInputKeyPrefix + sc.ID + ":9:s-x", Message: "hi", Attachments: "[]", Mode: InputModeQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
	if forged.Mode == InputModePatrol {
		t.Fatalf("EnqueueInput made a user input a patrol: %+v", forged)
	}
	fired := fireDueChatSchedules(t, s, dsn, time.Unix(sc.NextRunAt+1, 0))
	if len(fired) != 1 || fired[0].delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("fire = %+v; a forged user input must not make the patrol busy", fired)
	}
	paused := ChatScheduleStatusPaused
	if _, err := s.UpdateChatSchedule(ctx, user, conv.ID, sc.ID, ChatSchedulePatch{Status: &paused}); err != nil {
		t.Fatal(err)
	}
	if row := inputByID(t, s, conv.ID, "forged"); row.State != InputStateQueued {
		t.Fatalf("the user's own input was cancelled by pausing the schedule: %+v", row)
	}
}

// TestChatScheduleEditSupersedesAClaimedFire: a fire claimed before its
// schedule's definition was edited (a new prompt; a patrol turned into a
// timer) queues nothing with the old definition, writes no history, and the
// definition as edited fires at its slot. Pause and cancel keep revoking the
// fire, as before (TestChatSchedulePauseBetweenDeliveryAndSettle).
func TestChatScheduleEditSupersedesAClaimedFire(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	sd := openSchedOn(t, dsn)
	const user = "editor@elcanotek.com"
	patrolTurns := func(convID string) []InputQueueRow {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, inputQueueSelect+` WHERE conversation_id = $1 AND mode = 'patrol' ORDER BY position`, convID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []InputQueueRow
		for rows.Next() {
			r, err := scanInputRow(rows)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		patch func() ChatSchedulePatch
		want  string // text the next fire must carry
	}{
		{"new prompt", func() ChatSchedulePatch { p := "Check the NEW thing."; return ChatSchedulePatch{Prompt: &p} }, "Check the NEW thing."},
		{"patrol to timer", func() ChatSchedulePatch { d := 600; return ChatSchedulePatch{DurationSeconds: &d} }, "Scheduled timer"},
	} {
		conv, err := s.CreateConversation(ctx, user, tc.name, "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: user, Prompt: "Check the OLD thing.", CronExpr: "0 9 * * *"})
		if err != nil {
			t.Fatal(err)
		}
		at := time.Unix(sc.NextRunAt+1, 0)
		fires, err := sd.ClaimDueChatSchedules(ctx, at, time.Minute, 25, nil)
		if err != nil || len(fires) != 1 {
			t.Fatalf("%s: claim = %+v, %v", tc.name, fires, err)
		}
		edited, err := s.UpdateChatSchedule(ctx, user, conv.ID, sc.ID, tc.patch())
		if err != nil {
			t.Fatalf("%s: edit: %v", tc.name, err)
		}
		d, err := s.DeliverChatScheduleFire(ctx, fires[0], 20)
		if err != nil || d.Outcome != models.ChatScheduleSuperseded {
			t.Fatalf("%s: delivery of the stale fire = %+v, %v; want superseded", tc.name, d, err)
		}
		if turns := patrolTurns(conv.ID); len(turns) != 0 {
			t.Fatalf("%s: the stale fire queued %+v", tc.name, turns)
		}
		if _, err := sd.SettleChatScheduleFire(ctx, fires[0], d, at); err != nil {
			t.Fatal(err)
		}
		if runs, _ := sd.ListChatScheduleRuns(ctx, sc.ID, 10); len(runs) != 0 {
			t.Fatalf("%s: run history = %+v, want none", tc.name, runs)
		}
		all, _ := s.ListChatSchedules(ctx, user, conv.ID, true)
		if len(all) != 1 || all[0].RunCount != 0 {
			t.Fatalf("%s: schedule = %+v; want nothing counted", tc.name, all)
		}

		// The definition as edited fires at its slot.
		fired := fireDueChatSchedules(t, s, dsn, time.Unix(edited.NextRunAt+1, 0))
		if len(fired) != 1 || fired[0].delivery.Outcome != models.ChatScheduleQueued {
			t.Fatalf("%s: next fire = %+v", tc.name, fired)
		}
		turns := patrolTurns(conv.ID)
		if len(turns) != 1 || !strings.Contains(turns[0].Message, tc.want) || strings.Contains(turns[0].Message, "OLD thing") && tc.name == "new prompt" {
			t.Fatalf("%s: queued %+v; want one turn carrying %q", tc.name, turns, tc.want)
		}
	}
}

// TestChatScheduleEditAfterDeliveryCountsTheRun: an edit that lands after a
// fire's turn was delivered but before it settled does not supersede it:
// exactly one turn runs for that slot (the old prompt), it is counted and
// recorded, the slot advances by the edited cadence, and the next slot fires
// the edited definition.
func TestChatScheduleEditAfterDeliveryCountsTheRun(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	sd := openSchedOn(t, dsn)
	const user = "late-editor@elcanotek.com"
	conv, err := s.CreateConversation(ctx, user, "c", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: user, Prompt: "Check the OLD thing.", CronExpr: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(sc.NextRunAt+1, 0)
	fires, err := sd.ClaimDueChatSchedules(ctx, at, time.Minute, 25, nil)
	if err != nil || len(fires) != 1 {
		t.Fatalf("claim = %+v, %v", fires, err)
	}
	d, err := s.DeliverChatScheduleFire(ctx, fires[0], 20)
	if err != nil || d.Outcome != models.ChatScheduleQueued {
		t.Fatalf("deliver = %+v, %v", d, err)
	}
	newPrompt, newCron := "Check the NEW thing.", "0 */3 * * *"
	if _, err := s.UpdateChatSchedule(ctx, user, conv.ID, sc.ID, ChatSchedulePatch{Prompt: &newPrompt, CronExpr: &newCron}); err != nil {
		t.Fatal(err)
	}
	if ok, err := sd.SettleChatScheduleFire(ctx, fires[0], d, at); err != nil || !ok {
		t.Fatalf("settle = %v, %v", ok, err)
	}
	all, _ := s.ListChatSchedules(ctx, user, conv.ID, true)
	next, _ := models.NextChatScheduleRun(newCron, "UTC", at)
	if len(all) != 1 || all[0].RunCount != 1 || all[0].NextRunAt != next.Unix() {
		t.Fatalf("schedule = %+v; want the run counted and the slot at %d (edited cadence)", all, next.Unix())
	}
	if runs, _ := sd.ListChatScheduleRuns(ctx, sc.ID, 10); len(runs) != 1 || runs[0].InputID != d.InputID {
		t.Fatalf("history = %+v; want the run", runs)
	}
	// That slot is not fired again (by the edited definition or otherwise).
	if again := fireDueChatSchedules(t, s, dsn, at); len(again) != 0 {
		t.Fatalf("the slot fired again: %+v", again)
	}
	row := inputByID(t, s, conv.ID, d.InputID)
	if !strings.Contains(row.Message, "OLD thing") {
		t.Fatalf("the delivered turn = %q; want the old prompt", row.Message)
	}
	// The old turn finishes; the next slot fires the edited definition.
	if _, err := s.db.Exec(`UPDATE chat_input_queue SET state = 'completed' WHERE id = $1`, d.InputID); err != nil {
		t.Fatal(err)
	}
	fired := fireDueChatSchedules(t, s, dsn, time.Unix(all[0].NextRunAt+1, 0))
	if len(fired) != 1 || fired[0].delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("next slot = %+v", fired)
	}
	if row := inputByID(t, s, conv.ID, fired[0].delivery.InputID); !strings.Contains(row.Message, "NEW thing") {
		t.Fatalf("the next slot's turn = %q; want the edited prompt", row.Message)
	}
}
