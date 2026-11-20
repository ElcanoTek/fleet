package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestAdminSetChatSchedulePrompt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "owner@client.example"

	conv, err := s.CreateConversation(ctx, owner, "Opti Workbench", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: owner, Title: "Morning sweep",
		Prompt: "1. Scan.\n2. Offer a memory card for each change.", CronExpr: "30 6 * * 1-5", Timezone: "America/New_York",
	})
	if err != nil {
		t.Fatal(err)
	}
	cur, err := s.AdminGetChatSchedule(ctx, sc.ID[:MinChatScheduleIDPrefix])
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}
	if cur.ID != sc.ID || cur.ConversationTitle != "Opti Workbench" || cur.PromptSHA256 != PromptSHA256(sc.Prompt) {
		t.Fatalf("get = %+v", cur)
	}

	newPrompt := "1. Scan.\n2. Append each change to the shared team log."

	// A stale sha is refused and changes nothing.
	_, _, err = s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: PromptSHA256("something else"), Prompt: newPrompt, Notice: "n"})
	var changed *ChatSchedulePromptChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("stale sha err = %v, want ChatSchedulePromptChangedError", err)
	}
	// A missing sha is refused too.
	if _, _, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, Prompt: newPrompt}); !errors.Is(err, ErrInvalidChatSchedule) {
		t.Fatalf("missing sha err = %v", err)
	}
	if got, _ := s.AdminGetChatSchedule(ctx, sc.ID); got.Prompt != sc.Prompt || got.DefVersion != cur.DefVersion {
		t.Fatalf("a refused edit changed the schedule: %+v", got)
	}

	before, after, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{
		ID: sc.ID, ExpectSHA256: strings.ToUpper(cur.PromptSHA256), Prompt: "  " + newPrompt + "\n",
		Notice: "An administrator updated the instructions. Reason: team log moved.",
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if before.Prompt != sc.Prompt || after.Prompt != newPrompt {
		t.Fatalf("before/after = %q / %q", before.Prompt, after.Prompt)
	}
	if after.DefVersion != before.DefVersion+1 {
		t.Fatalf("def_version %d -> %d, want a bump", before.DefVersion, after.DefVersion)
	}
	if after.Status != ChatScheduleStatusActive || after.CronExpr != sc.CronExpr || after.NextRunAt != sc.NextRunAt || after.UserEmail != owner || after.Title != "Morning sweep" {
		t.Fatalf("the edit touched more than the prompt: %+v", after)
	}

	hist, notices := chatNotices(t, s, conv.ID)
	if len(notices) != 1 || notices[0].Kind != NoticePatrolUpdated || !strings.Contains(notices[0].Text, "team log moved") {
		t.Fatalf("notices = %+v", notices)
	}

	// Re-applying the same prompt is a no-op: no bump, no second notice.
	_, again, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: after.PromptSHA256, Prompt: newPrompt, Notice: "again"})
	if err != nil || again.DefVersion != after.DefVersion {
		t.Fatalf("no-op = %+v, %v", again, err)
	}
	if hist2, _ := s.LoadHistory(ctx, conv.ID); len(hist2) != len(hist) {
		t.Fatalf("a no-op wrote history")
	}

	// A title edit renames; a blank title keeps the old one.
	title := "Morning sweep (team log)"
	_, renamed, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: after.PromptSHA256, Prompt: newPrompt, Title: &title})
	if err != nil || renamed.Title != title || renamed.DefVersion != after.DefVersion+1 {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}

	// The owner's own path still works on the edited row.
	p := "Owner edit."
	if _, err := s.UpdateChatSchedule(ctx, owner, conv.ID, sc.ID, ChatSchedulePatch{Prompt: &p}); err != nil {
		t.Fatalf("owner edit after admin edit: %v", err)
	}

	// Cancelled schedules are not editable.
	if _, err := s.CancelChatSchedule(ctx, owner, conv.ID, sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: PromptSHA256(p), Prompt: "x"}); !errors.Is(err, ErrInvalidChatSchedule) {
		t.Fatalf("edit of a cancelled schedule err = %v", err)
	}
}

// chatNotices returns a conversation's history and the notices in it.
func chatNotices(t *testing.T, s *Store, convID string) ([]agent.HistoryEntry, []agent.NoticeContent) {
	t.Helper()
	hist, err := s.LoadHistory(context.Background(), convID)
	if err != nil {
		t.Fatal(err)
	}
	var notices []agent.NoticeContent
	for _, e := range hist {
		if e.Type != agent.EntryTypeNotice {
			continue
		}
		var n agent.NoticeContent
		if err := json.Unmarshal(e.Content, &n); err != nil {
			t.Fatal(err)
		}
		notices = append(notices, n)
	}
	return hist, notices
}

func TestAdminChatScheduleLookupAndList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.AdminGetChatSchedule(ctx, "short"); !errors.Is(err, ErrInvalidChatSchedule) {
		t.Fatalf("short prefix err = %v", err)
	}
	if _, err := s.AdminGetChatSchedule(ctx, "zzzzzzzz-not-there"); !errors.Is(err, ErrChatScheduleNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	// LIKE metacharacters in a prefix match literally, not as wildcards.
	if _, err := s.AdminGetChatSchedule(ctx, "%%%%%%%%"); !errors.Is(err, ErrChatScheduleNotFound) {
		t.Fatalf("wildcard prefix err = %v", err)
	}

	mk := func(owner, title, cron string, dur int) ChatSchedule {
		conv, err := s.CreateConversation(ctx, owner, title, "victoria", "", false)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: owner, Prompt: "p " + title, CronExpr: cron, DurationSeconds: dur})
		if err != nil {
			t.Fatal(err)
		}
		return *sc
	}
	a := mk("a@x.example", "A chat", "0 9 * * *", 0)
	_ = mk("b@x.example", "B chat", "0 10 * * *", 0)
	_ = mk("b@x.example", "B timer", "", 600)

	// Two ids sharing the first 8 characters are ambiguous at that prefix.
	if _, err := s.db.ExecContext(ctx, `UPDATE `+chatSchedulesTable+` SET id = 'abcdefgh-1' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	c := mk("c@x.example", "C chat", "0 11 * * *", 0)
	if _, err := s.db.ExecContext(ctx, `UPDATE `+chatSchedulesTable+` SET id = 'abcdefgh-2' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminGetChatSchedule(ctx, "abcdefgh"); !errors.Is(err, ErrChatScheduleAmbiguous) {
		t.Fatalf("ambiguous err = %v", err)
	}
	if got, err := s.AdminGetChatSchedule(ctx, "abcdefgh-2"); err != nil || got.ConversationTitle != "C chat" {
		t.Fatalf("exact id = %+v, %v", got, err)
	}

	all, err := s.AdminListChatSchedules(ctx, AdminChatScheduleFilter{})
	if err != nil || len(all) != 4 {
		t.Fatalf("list all = %d, %v", len(all), err)
	}
	patrols, _ := s.AdminListChatSchedules(ctx, AdminChatScheduleFilter{PatrolsOnly: true})
	if len(patrols) != 3 {
		t.Fatalf("patrols only = %d", len(patrols))
	}
	bs, _ := s.AdminListChatSchedules(ctx, AdminChatScheduleFilter{UserEmail: "B@X.example", PatrolsOnly: true})
	if len(bs) != 1 || bs[0].ConversationTitle != "B chat" {
		t.Fatalf("by user = %+v", bs)
	}
	byConv, _ := s.AdminListChatSchedules(ctx, AdminChatScheduleFilter{ConversationID: bs[0].ConversationID[:8]})
	if len(byConv) != 1 {
		t.Fatalf("by conversation prefix = %d", len(byConv))
	}
}

// An operator edit supersedes a fire claimed under the old prompt, exactly as
// the owner's edit does, so the next run carries the new prompt.
func TestAdminSetChatSchedulePromptSupersedesClaimedFire(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	sd := openSchedOn(t, dsn)
	ctx := context.Background()
	const owner = "owner@client.example"

	conv, err := s.CreateConversation(ctx, owner, "Workbench", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: owner, Prompt: "Check the OLD thing.", CronExpr: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(sc.NextRunAt+1, 0)
	fires, err := sd.ClaimDueChatSchedules(ctx, at, time.Minute, 25, nil)
	if err != nil || len(fires) != 1 {
		t.Fatalf("claim = %+v, %v", fires, err)
	}
	cur, _ := s.AdminGetChatSchedule(ctx, sc.ID)
	if !cur.FireInFlight {
		t.Fatalf("claimed fire not reported in flight: %+v", cur)
	}
	if _, _, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: cur.PromptSHA256, Prompt: "Check the NEW thing."}); err != nil {
		t.Fatal(err)
	}
	d, err := s.DeliverChatScheduleFire(ctx, fires[0], 20)
	if err != nil || d.Outcome != models.ChatScheduleSuperseded {
		t.Fatalf("delivery of the stale fire = %+v, %v; want superseded", d, err)
	}
	if _, err := sd.SettleChatScheduleFire(ctx, fires[0], d, at); err != nil {
		t.Fatal(err)
	}
	next := fireDueChatSchedules(t, s, dsn, at.Add(2*time.Second))
	if len(next) != 1 || next[0].delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("re-fire = %+v", next)
	}
	rows, err := s.db.QueryContext(ctx, inputQueueSelect+` WHERE conversation_id = $1`, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var texts []string
	for rows.Next() {
		r, err := scanInputRow(rows)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, r.Message)
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "Check the NEW thing.") || strings.Contains(texts[0], "OLD") {
		t.Fatalf("queued patrol input = %q, want the new prompt only", texts)
	}
}

// A run already delivered into the chat's queue (waiting behind a running
// turn) carries the prompt it was claimed with; an operator edit rewrites it
// so it runs the new instructions, not the old ones once more.
func TestAdminSetChatSchedulePromptRewritesQueuedRuns(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	const owner = "owner@client.example"
	conv, err := s.CreateConversation(ctx, owner, "Workbench", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: owner, Title: "Sweep", Prompt: "Offer a memory card.", CronExpr: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	fired := fireDueChatSchedules(t, s, dsn, time.Unix(sc.NextRunAt+1, 0))
	if len(fired) != 1 || fired[0].delivery.Outcome != models.ChatScheduleQueued {
		t.Fatalf("fire = %+v", fired)
	}
	cur, _ := s.AdminGetChatSchedule(ctx, sc.ID)
	_, after, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: cur.PromptSHA256, Prompt: "Append to the shared team log."})
	if err != nil {
		t.Fatal(err)
	}
	if after.QueuedRunsUpdated != 1 {
		t.Fatalf("queued runs updated = %d, want 1", after.QueuedRunsUpdated)
	}
	queued, err := s.ListQueuedInputs(ctx, "", conv.ID)
	if err != nil || len(queued) != 1 {
		t.Fatalf("queue = %+v, %v", queued, err)
	}
	if !strings.Contains(queued[0].Message, "Append to the shared team log.") || strings.Contains(queued[0].Message, "memory card") || !strings.Contains(queued[0].Message, "(run #1)") {
		t.Fatalf("queued run message = %q", queued[0].Message)
	}
}

// A run already claimed by the queue's drain (running) keeps its message; the
// edit reports it instead of implying the old instructions can no longer run.
func TestAdminSetChatSchedulePromptReportsRunningRun(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	ctx := context.Background()
	const owner = "owner@client.example"
	conv, err := s.CreateConversation(ctx, owner, "Workbench", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: owner, Prompt: "old", CronExpr: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	fireDueChatSchedules(t, s, dsn, time.Unix(sc.NextRunAt+1, 0))
	if row, err := s.ClaimNextQueuedInput(ctx, conv.ID, "turn-1"); err != nil || row == nil {
		t.Fatalf("claim = %v, %v", row, err)
	}
	cur, _ := s.AdminGetChatSchedule(ctx, sc.ID)
	_, after, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: cur.PromptSHA256, Prompt: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if !after.RunningWithOldPrompt || after.QueuedRunsUpdated != 0 {
		t.Fatalf("after = running %t, rewritten %d", after.RunningWithOldPrompt, after.QueuedRunsUpdated)
	}
}

// A soft-deleted chat's schedules are not listed, shown or editable.
func TestAdminChatSchedulesHideDeletedChats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "owner@client.example"
	conv, err := s.CreateConversation(ctx, owner, "Gone", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: owner, Prompt: "p", CronExpr: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET deleted_at = now() WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.AdminListChatSchedules(ctx, AdminChatScheduleFilter{}); len(list) != 0 {
		t.Fatalf("listed %d schedules of a deleted chat", len(list))
	}
	if _, err := s.AdminGetChatSchedule(ctx, sc.ID); !errors.Is(err, ErrChatScheduleNotFound) {
		t.Fatalf("get err = %v", err)
	}
	if _, _, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: PromptSHA256("p"), Prompt: "x"}); !errors.Is(err, ErrChatScheduleNotFound) {
		t.Fatalf("edit err = %v", err)
	}
}

type adminCronFixture struct {
	s     *Store
	ctx   context.Context
	admin AdminActor
	conv  *Conversation
	sc    *ChatSchedule
	v     int64
}

const adminCronOwner = "owner@client.example"

// newAdminCronFixture is a weekday 06:30 New York patrol, like the client
// patrol this feature was built for.
func newAdminCronFixture(t *testing.T) adminCronFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, adminCronOwner, "Opti Workbench", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: adminCronOwner, Title: "Pre-Loaded Opti Workbench",
		Prompt: "Scan the workbench.", CronExpr: "30 6 * * 1-5", Timezone: "America/New_York",
	})
	if err != nil {
		t.Fatal(err)
	}
	cur, err := s.AdminGetChatSchedule(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return adminCronFixture{s: s, ctx: ctx, admin: AdminActor{Email: "ops@elcano.example", Via: AdminViaCLI}, conv: conv, sc: sc, v: cur.DefVersion}
}

// requireUnchanged fails when anything was written: schedule, notice or record.
func (f adminCronFixture) requireUnchanged(t *testing.T, what string) {
	t.Helper()
	got, err := f.s.AdminGetChatSchedule(f.ctx, f.sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CronExpr != f.sc.CronExpr || got.Timezone != f.sc.Timezone || got.NextRunAt != f.sc.NextRunAt || got.DefVersion != f.v {
		t.Fatalf("%s changed the schedule: %+v", what, got)
	}
	if _, notices := chatNotices(t, f.s, f.conv.ID); len(notices) != 0 {
		t.Fatalf("%s wrote a notice: %+v", what, notices)
	}
	if acts, _ := f.s.ListAdminActions(f.ctx, f.conv.ID, 10); len(acts) != 0 {
		t.Fatalf("%s recorded an action: %+v", what, acts)
	}
}

func TestAdminSetChatScheduleCronRefusals(t *testing.T) {
	t.Parallel()
	f := newAdminCronFixture(t)
	v, id := f.v, f.sc.ID
	refusals := []struct {
		name string
		in   AdminChatScheduleCronEdit
		want error
	}{
		{"no expected def_version", AdminChatScheduleCronEdit{ID: id, CronExpr: "30 6 * * *", Reason: "r"}, ErrInvalidChatSchedule},
		{"no reason", AdminChatScheduleCronEdit{ID: id, CronExpr: "30 6 * * *", ExpectDefVersion: &v}, ErrAdminReasonRequired},
		{"nothing to change", AdminChatScheduleCronEdit{ID: id, ExpectDefVersion: &v, Reason: "r"}, ErrInvalidChatSchedule},
		{"invalid cron", AdminChatScheduleCronEdit{ID: id, CronExpr: "30 6 * *", ExpectDefVersion: &v, Reason: "r"}, ErrInvalidChatSchedule},
		{"invalid timezone", AdminChatScheduleCronEdit{ID: id, Timezone: "Mars/Olympus_Mons", ExpectDefVersion: &v, Reason: "r"}, ErrInvalidChatSchedule},
		{"too frequent", AdminChatScheduleCronEdit{ID: id, CronExpr: "* * * * *", ExpectDefVersion: &v, Reason: "r"}, ErrInvalidChatSchedule},
		{"invalid cron on a dry run", AdminChatScheduleCronEdit{ID: id, CronExpr: "61 * * * *", ExpectDefVersion: &v, DryRun: true}, ErrInvalidChatSchedule},
	}
	for _, r := range refusals {
		if _, err := f.s.AdminSetChatScheduleCron(f.ctx, f.admin, r.in); !errors.Is(err, r.want) {
			t.Fatalf("%s: err = %v, want %v", r.name, err, r.want)
		}
	}
	stale := v + 7
	var changed *ChatScheduleDefinitionChangedError
	if _, err := f.s.AdminSetChatScheduleCron(f.ctx, f.admin, AdminChatScheduleCronEdit{ID: id, CronExpr: "30 6 * * *", ExpectDefVersion: &stale, Reason: "r"}); !errors.As(err, &changed) || changed.Current != v {
		t.Fatalf("stale def_version err = %v, want ChatScheduleDefinitionChangedError", err)
	}
	if _, err := f.s.AdminSetChatScheduleCron(f.ctx, AdminActor{}, AdminChatScheduleCronEdit{ID: id, CronExpr: "30 6 * * *", ExpectDefVersion: &v, Reason: "r"}); err == nil {
		t.Fatal("an edit without an actor was accepted")
	}
	f.requireUnchanged(t, "a refused edit")
}

// A dry run needs no reason, runs every check, previews the next runs in the
// patrol's timezone, and changes nothing.
func TestAdminSetChatScheduleCronDryRun(t *testing.T) {
	t.Parallel()
	f := newAdminCronFixture(t)
	dry, err := f.s.AdminSetChatScheduleCron(f.ctx, f.admin, AdminChatScheduleCronEdit{ID: f.sc.ID[:MinChatScheduleIDPrefix], CronExpr: " 30 6 * * * ", ExpectDefVersion: &f.v, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.Changed || !dry.DryRun || dry.After.CronExpr != "30 6 * * *" || dry.After.Timezone != "America/New_York" || len(dry.NextRuns) != AdminCronPreviewRuns {
		t.Fatalf("dry run = %+v", dry)
	}
	ny := mustLoc(t, "America/New_York")
	for i, r := range dry.NextRuns {
		if at := time.Unix(r, 0).In(ny); at.Hour() != 6 || at.Minute() != 30 {
			t.Fatalf("preview run %d = %s, want 06:30 New York", i, at)
		}
		if i > 0 && r <= dry.NextRuns[i-1] {
			t.Fatalf("preview runs out of order: %v", dry.NextRuns)
		}
	}
	f.requireUnchanged(t, "a dry run")
}

func TestAdminSetChatScheduleCron(t *testing.T) {
	t.Parallel()
	f := newAdminCronFixture(t)
	s, ctx, sc, v := f.s, f.ctx, f.sc, f.v
	res, err := s.AdminSetChatScheduleCron(ctx, f.admin, AdminChatScheduleCronEdit{ID: sc.ID, CronExpr: "30 6 * * *", ExpectDefVersion: &v, Reason: "  the team works weekends too "})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	after := res.After
	if !res.Changed || after.CronExpr != "30 6 * * *" || after.Timezone != "America/New_York" || after.DefVersion != v+1 {
		t.Fatalf("after = %+v", after)
	}
	if after.NextRunAt != res.NextRuns[0] || after.NextRunAt <= time.Now().Unix() {
		t.Fatalf("next run %d, preview %v", after.NextRunAt, res.NextRuns)
	}
	if after.UserEmail != adminCronOwner || after.Prompt != sc.Prompt || after.Title != sc.Title || after.Status != ChatScheduleStatusActive {
		t.Fatalf("the edit touched more than the schedule: %+v", after)
	}
	_, notices := chatNotices(t, s, f.conv.ID)
	if len(notices) != 1 || notices[0].Kind != NoticePatrolUpdated || notices[0].Text != res.Notice ||
		!strings.Contains(res.Notice, `from "30 6 * * 1-5" (America/New_York) to "30 6 * * *" (America/New_York)`) ||
		!strings.Contains(res.Notice, "Reason: the team works weekends too") {
		t.Fatalf("notices = %+v", notices)
	}
	acts, err := s.ListAdminActions(ctx, f.conv.ID, 10)
	if err != nil || len(acts) != 1 {
		t.Fatalf("actions = %+v, %v", acts, err)
	}
	if a := acts[0]; a.Action != "patrol_schedule" || a.Actor != f.admin.Email || a.Via != AdminViaCLI || a.ScheduleID != sc.ID ||
		a.Reason != "the team works weekends too" || !strings.Contains(a.Detail, `"30 6 * * 1-5" (America/New_York) -> "30 6 * * *" (America/New_York)`) {
		t.Fatalf("action = %+v", a)
	}

	// The same schedule again is a no-op: no bump, no second notice or record.
	v2 := after.DefVersion
	again, err := s.AdminSetChatScheduleCron(ctx, f.admin, AdminChatScheduleCronEdit{ID: sc.ID, CronExpr: "30 6 * * *", Timezone: "America/New_York", ExpectDefVersion: &v2, Reason: "again"})
	if err != nil || again.Changed || again.After.DefVersion != v2 {
		t.Fatalf("no-op = %+v, %v", again, err)
	}
	if _, n := chatNotices(t, s, f.conv.ID); len(n) != 1 {
		t.Fatalf("a no-op wrote a notice")
	}
	// The edit before is now stale.
	var changed *ChatScheduleDefinitionChangedError
	if _, err := s.AdminSetChatScheduleCron(ctx, f.admin, AdminChatScheduleCronEdit{ID: sc.ID, Timezone: "UTC", ExpectDefVersion: &v, Reason: "r"}); !errors.As(err, &changed) {
		t.Fatalf("edit against the old def_version err = %v", err)
	}
}

// A timezone alone moves the patrol to that zone, keeping its cron.
func TestAdminSetChatScheduleCronTimezoneOnly(t *testing.T) {
	t.Parallel()
	f := newAdminCronFixture(t)
	tz, err := f.s.AdminSetChatScheduleCron(f.ctx, f.admin, AdminChatScheduleCronEdit{ID: f.sc.ID, Timezone: "Europe/London", ExpectDefVersion: &f.v, Reason: "team moved"})
	if err != nil || tz.After.CronExpr != "30 6 * * 1-5" || tz.After.Timezone != "Europe/London" || tz.After.DefVersion != f.v+1 {
		t.Fatalf("timezone edit = %+v, %v", tz.After, err)
	}
	if at := time.Unix(tz.After.NextRunAt, 0).In(mustLoc(t, "Europe/London")); at.Hour() != 6 || at.Minute() != 30 {
		t.Fatalf("next run %s, want 06:30 London", at)
	}
}

// A paused patrol keeps its status and next run (a resume computes it); its
// new schedule is still validated.
func TestAdminSetChatScheduleCronPaused(t *testing.T) {
	t.Parallel()
	f := newAdminCronFixture(t)
	s, ctx, sc := f.s, f.ctx, f.sc
	paused := ChatScheduleStatusPaused
	if _, err := s.UpdateChatSchedule(ctx, adminCronOwner, f.conv.ID, sc.ID, ChatSchedulePatch{Status: &paused}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.AdminGetChatSchedule(ctx, sc.ID)
	pv := p.DefVersion
	if _, err := s.AdminSetChatScheduleCron(ctx, f.admin, AdminChatScheduleCronEdit{ID: sc.ID, CronExpr: "*/2 * * * *", ExpectDefVersion: &pv, Reason: "r"}); !errors.Is(err, ErrInvalidChatSchedule) {
		t.Fatalf("too-frequent cron on a paused patrol err = %v", err)
	}
	pr, err := s.AdminSetChatScheduleCron(ctx, f.admin, AdminChatScheduleCronEdit{ID: sc.ID, CronExpr: "0 7 * * *", ExpectDefVersion: &pv, Reason: "later"})
	if err != nil || pr.After.Status != ChatScheduleStatusPaused || pr.After.NextRunAt != p.NextRunAt || pr.After.CronExpr != "0 7 * * *" {
		t.Fatalf("paused edit = %+v, %v", pr.After, err)
	}
	active := ChatScheduleStatusActive
	resumed, err := s.UpdateChatSchedule(ctx, adminCronOwner, f.conv.ID, sc.ID, ChatSchedulePatch{Status: &active})
	if err != nil {
		t.Fatal(err)
	}
	if at := time.Unix(resumed.NextRunAt, 0).In(mustLoc(t, "America/New_York")); at.Hour() != 7 || at.Minute() != 0 {
		t.Fatalf("resumed next run %s, want 07:00 New York", at)
	}
}

// Cancelled patrols and one-shot timers have no schedule to change.
func TestAdminSetChatScheduleCronRefusesCancelledAndTimers(t *testing.T) {
	t.Parallel()
	f := newAdminCronFixture(t)
	s, ctx, sc := f.s, f.ctx, f.sc
	if _, err := s.CancelChatSchedule(ctx, adminCronOwner, f.conv.ID, sc.ID); err != nil {
		t.Fatal(err)
	}
	c, _ := s.AdminGetChatSchedule(ctx, sc.ID)
	cv := c.DefVersion
	if _, err := s.AdminSetChatScheduleCron(ctx, f.admin, AdminChatScheduleCronEdit{ID: sc.ID, CronExpr: "0 8 * * *", ExpectDefVersion: &cv, Reason: "r"}); !errors.Is(err, ErrInvalidChatSchedule) {
		t.Fatalf("edit of a cancelled patrol err = %v", err)
	}
	timer, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: f.conv.ID, UserEmail: adminCronOwner, Prompt: "ping", DurationSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	var zero int64
	if _, err := s.AdminSetChatScheduleCron(ctx, f.admin, AdminChatScheduleCronEdit{ID: timer.ID, CronExpr: "0 8 * * *", ExpectDefVersion: &zero, Reason: "r"}); !errors.Is(err, ErrInvalidChatSchedule) || !strings.Contains(err.Error(), "timer") {
		t.Fatalf("edit of a timer err = %v", err)
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The owner's own edit validates a paused patrol's new cadence too (the
// path the administrator's edit shares): a cron that cannot run is refused
// when it is set, not when someone later tries to resume the patrol.
func TestUpdateChatScheduleValidatesPausedCron(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "owner@client.example"
	conv, err := s.CreateConversation(ctx, owner, "Workbench", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: owner, Prompt: "p", CronExpr: "0 9 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	paused := ChatScheduleStatusPaused
	if _, err := s.UpdateChatSchedule(ctx, owner, conv.ID, sc.ID, ChatSchedulePatch{Status: &paused}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"not a cron", "* * * * *"} {
		if _, err := s.UpdateChatSchedule(ctx, owner, conv.ID, sc.ID, ChatSchedulePatch{CronExpr: &bad}); !errors.Is(err, ErrInvalidChatSchedule) {
			t.Fatalf("paused cron %q err = %v", bad, err)
		}
	}
	good := "CRON_TZ=UTC 0 10 * * *"
	got, err := s.UpdateChatSchedule(ctx, owner, conv.ID, sc.ID, ChatSchedulePatch{CronExpr: &good})
	if err != nil || got.CronExpr != "0 10 * * *" || got.Status != ChatScheduleStatusPaused || got.NextRunAt != sc.NextRunAt {
		t.Fatalf("paused edit = %+v, %v", got, err)
	}
}

// An administrator's schedule edit supersedes a fire claimed under the old
// schedule, exactly as the owner's edit does.
func TestAdminSetChatScheduleCronSupersedesClaimedFire(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	sd := openSchedOn(t, dsn)
	ctx := context.Background()
	const owner = "owner@client.example"
	conv, err := s.CreateConversation(ctx, owner, "Workbench", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: conv.ID, UserEmail: owner, Prompt: "Check.", CronExpr: "0 9 * * 1-5"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(sc.NextRunAt+1, 0)
	fires, err := sd.ClaimDueChatSchedules(ctx, at, time.Minute, 25, nil)
	if err != nil || len(fires) != 1 {
		t.Fatalf("claim = %+v, %v", fires, err)
	}
	cur, _ := s.AdminGetChatSchedule(ctx, sc.ID)
	v := cur.DefVersion
	if _, err := s.AdminSetChatScheduleCron(ctx, AdminActor{Email: "ops@x", Via: AdminViaWeb}, AdminChatScheduleCronEdit{ID: sc.ID, CronExpr: "0 9 * * *", ExpectDefVersion: &v, Reason: "every day"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.DeliverChatScheduleFire(ctx, fires[0], 20)
	if err != nil || d.Outcome != models.ChatScheduleSuperseded {
		t.Fatalf("delivery of the stale fire = %+v, %v; want superseded", d, err)
	}
	if queued, _ := s.ListQueuedInputs(ctx, "", conv.ID); len(queued) != 0 {
		t.Fatalf("the superseded fire queued %d run(s)", len(queued))
	}
}
