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
