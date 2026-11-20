package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

var webAdmin = AdminActor{Email: "alex@ops.example", Via: AdminViaWeb}

// A pinned note is set, replaced and cleared; each change is recorded; and it
// never becomes part of the chat's history, which is all the model reads.
func TestAdminNote_SetReplaceClear(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	c, err := s.CreateConversation(ctx, "dana@client.example", "Weekly report", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.GetAdminNote(ctx, c.ID); err != nil || n != nil {
		t.Fatalf("a new chat has no note: %+v, %v", n, err)
	}
	if _, err := s.AdminSetNote(ctx, webAdmin, c.ID, "   "); !errors.Is(err, ErrAdminNoteEmpty) {
		t.Fatalf("empty note err = %v", err)
	}
	if _, err := s.AdminSetNote(ctx, webAdmin, c.ID, strings.Repeat("x", MaxAdminNoteRunes+1)); !errors.Is(err, ErrAdminNoteTooLong) {
		t.Fatalf("long note err = %v", err)
	}
	if _, err := s.AdminSetNote(ctx, AdminActor{}, c.ID, "hello"); err == nil {
		t.Fatal("a note needs an actor")
	}

	first, err := s.AdminSetNote(ctx, webAdmin, c.ID, "  I fixed the 06:30 patrol.\nReply here if it fails again.  ")
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != "I fixed the 06:30 patrol.\nReply here if it fails again." || first.Author != webAdmin.Email || first.Via != AdminViaWeb || first.SeenAt != nil {
		t.Fatalf("first note = %+v", first)
	}

	// The same text again changes nothing and records nothing.
	same, err := s.AdminSetNote(ctx, cliAdmin, c.ID, first.Text)
	if err != nil || same.UpdatedAt != first.UpdatedAt || same.Author != webAdmin.Email {
		t.Fatalf("re-set = %+v, %v", same, err)
	}

	// The owner reads it.
	seen, err := s.AckAdminNote(ctx, c.ID, "Dana@client.example", first.UpdatedAt)
	if err != nil || seen == nil || seen.SeenAt == nil || seen.SeenBy != "dana@client.example" {
		t.Fatalf("ack = %+v, %v", seen, err)
	}
	// A second acknowledgement does not replace the first.
	again, _ := s.AckAdminNote(ctx, c.ID, "other@client.example", first.UpdatedAt)
	if again.SeenBy != "dana@client.example" {
		t.Fatalf("second ack overwrote the first: %+v", again)
	}

	// New text is unread, and an acknowledgement of the old text does not
	// mark it.
	second, err := s.AdminSetNote(ctx, cliAdmin, c.ID, "Try the Monday run again.")
	if err != nil {
		t.Fatal(err)
	}
	if second.SeenAt != nil || second.SeenBy != "" || second.UpdatedAt <= first.UpdatedAt || second.CreatedAt != first.CreatedAt || second.Author != cliAdmin.Email || second.Via != AdminViaCLI {
		t.Fatalf("replaced note = %+v (first %+v)", second, first)
	}
	stale, _ := s.AckAdminNote(ctx, c.ID, "dana@client.example", first.UpdatedAt)
	if stale == nil || stale.SeenAt != nil {
		t.Fatalf("an acknowledgement of replaced text marked the new text: %+v", stale)
	}

	// The note is not history: the model's context never carries it.
	hist, err := s.LoadHistory(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range hist {
		if strings.Contains(string(e.Content), "Monday run") || strings.Contains(string(e.Content), "06:30 patrol") {
			t.Fatalf("the pinned note reached the chat's history: %s", e.Content)
		}
	}

	listed, err := s.AdminGetChat(ctx, c.ID)
	if err != nil || !listed.HasNote || listed.NoteSeen {
		t.Fatalf("chat = %+v, %v; want has_note, unseen", listed, err)
	}

	if ok, err := s.AdminClearNote(ctx, webAdmin, c.ID); err != nil || !ok {
		t.Fatalf("clear = %t, %v", ok, err)
	}
	if ok, err := s.AdminClearNote(ctx, webAdmin, c.ID); err != nil || ok {
		t.Fatalf("second clear = %t, %v; want nothing to clear", ok, err)
	}
	if n, _ := s.GetAdminNote(ctx, c.ID); n != nil {
		t.Fatalf("note after clear = %+v", n)
	}
	if got, _ := s.AckAdminNote(ctx, c.ID, "dana@client.example", second.UpdatedAt); got != nil {
		t.Fatalf("ack of a cleared note = %+v", got)
	}

	actions, err := s.ListAdminActions(ctx, c.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i := len(actions) - 1; i >= 0; i-- {
		kinds = append(kinds, actions[i].Action)
	}
	if got := strings.Join(kinds, ","); got != "note_set,note_replace,note_clear" {
		t.Fatalf("recorded actions = %s", got)
	}
}

// A note cannot be pinned to a chat in the recycle bin, and goes with a chat
// deleted for good.
func TestAdminNote_DeletedChats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	c, err := s.CreateConversation(ctx, "dana@client.example", "Gone", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetNote(ctx, webAdmin, c.ID, "Keep me."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminMoveChatToBin(ctx, c.ID, "binned"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetNote(ctx, webAdmin, c.ID, "Too late."); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("note on a binned chat err = %v", err)
	}
	// The bin keeps the note, so a restore gives the chat back whole.
	if n, _ := s.GetAdminNote(ctx, c.ID); n == nil || n.Text != "Keep me." {
		t.Fatalf("note in the bin = %+v", n)
	}
	if err := s.AdminPurgeChat(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.GetAdminNote(ctx, c.ID); n != nil {
		t.Fatalf("note after a permanent delete = %+v", n)
	}
}

// The listing's attention signals, its state filter, the per-owner overview
// and the waiting cards agree with one another.
func TestAdminChats_AttentionStatesAndOverview(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	mk := func(owner, title string) *Conversation {
		t.Helper()
		c, err := s.CreateConversation(ctx, owner, title, "victoria", "m1", false)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	stuck := mk("dana@client.example", "Send the weekly report")
	failed := mk("dana@client.example", "Budget import")
	fine := mk("dana@client.example", "Scratch")
	running := mk("li@client.example", "Pacing sweep")
	archived := mk("li@client.example", "Old plan")
	binned := mk("sam@client.example", "Mistake")

	now := time.Now().Unix()
	card, err := s.CreateApproval(ctx, stuck.ID, "dana@client.example", "send_email", "call_1", `{}`, now+3600, ApprovalSeat{})
	if err != nil {
		t.Fatal(err)
	}
	turn := func(conv *Conversation, id string, status TurnStatus) {
		t.Helper()
		if err := s.CreateTurn(ctx, id, conv.ID, now); err != nil {
			t.Fatal(err)
		}
		if status != TurnStatusRunning {
			if err := s.FinishTurn(ctx, id, status, now+1, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	turn(failed, "t-failed", TurnStatusError)
	turn(fine, "t-fine", TurnStatusCompleted)
	turn(running, "t-running", TurnStatusRunning)
	if _, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: running.ID, UserEmail: "li@client.example", Prompt: "sweep", CronExpr: "30 6 * * 1-5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminSetNote(ctx, webAdmin, fine.ID, "Welcome."); err != nil {
		t.Fatal(err)
	}
	if err := s.SetArchived(ctx, "li@client.example", archived.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminMoveChatToBin(ctx, binned.ID, "binned"); err != nil {
		t.Fatal(err)
	}

	ids := func(f AdminChatFilter) string {
		t.Helper()
		chats, err := s.AdminListChats(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		titles := make([]string, 0, len(chats))
		for _, c := range chats {
			titles = append(titles, c.Title)
		}
		// Order-independent: the test's chats share one updated_at second.
		for i := range titles {
			for j := i + 1; j < len(titles); j++ {
				if titles[j] < titles[i] {
					titles[i], titles[j] = titles[j], titles[i]
				}
			}
		}
		return strings.Join(titles, "|")
	}
	for state, want := range map[string]string{
		"":                      "Budget import|Pacing sweep|Scratch|Send the weekly report",
		AdminChatStateAttention: "Budget import|Send the weekly report",
		AdminChatStateRunning:   "Pacing sweep",
		AdminChatStatePatrols:   "Pacing sweep",
		AdminChatStateNoted:     "Scratch",
		AdminChatStateArchived:  "Old plan",
	} {
		if got := ids(AdminChatFilter{State: state}); got != want {
			t.Errorf("state %q lists %q, want %q", state, got, want)
		}
	}
	if _, err := s.AdminListChats(ctx, AdminChatFilter{State: "nonsense"}); !errors.Is(err, ErrAdminChatState) {
		t.Fatalf("unknown state err = %v", err)
	}
	// A search matches the owner's email too.
	if got := ids(AdminChatFilter{Search: "LI@client"}); got != "Pacing sweep" {
		t.Errorf("search by owner lists %q", got)
	}

	got, err := s.AdminGetChat(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Attention, ",") != AdminAttentionCardsWaiting || got.WaitingSince != card.CreatedAt {
		t.Fatalf("stuck chat = attention %v, waiting since %d (card %d)", got.Attention, got.WaitingSince, card.CreatedAt)
	}
	if f, _ := s.AdminGetChat(ctx, failed.ID); strings.Join(f.Attention, ",") != AdminAttentionTurnFailed || f.LastTurnStatus != "error" {
		t.Fatalf("failed chat = %+v", f)
	}
	if f, _ := s.AdminGetChat(ctx, fine.ID); len(f.Attention) != 0 || !f.HasNote {
		t.Fatalf("fine chat = %+v", f)
	}

	cards, err := s.AdminListPendingCards(ctx, stuck.ID)
	if err != nil || len(cards) != 1 || cards[0].Tool != "send_email" || cards[0].ForEmail != "dana@client.example" || cards[0].ExpiresAt != now+3600 {
		t.Fatalf("pending cards = %+v, %v", cards, err)
	}

	o, err := s.AdminChatOverview(ctx, AdminChatFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := AdminChatCounts{Chats: 4, Attention: 2, Running: 1, Patrols: 1, Noted: 1, CardsWaiting: 1}
	if o.AdminChatCounts != want || o.Archived != 1 || o.Deleted != 1 {
		t.Fatalf("overview = %+v archived=%d deleted=%d, want %+v 1 1", o.AdminChatCounts, o.Archived, o.Deleted, want)
	}
	// People needing attention first.
	if len(o.People) != 2 || o.People[0].Email != "dana@client.example" || o.People[0].Chats != 3 || o.People[0].Attention != 2 ||
		o.People[1].Email != "li@client.example" || o.People[1].Chats != 1 || o.People[1].Running != 1 {
		t.Fatalf("people = %+v", o.People)
	}
	// Including archived chats widens the scope; one owner narrows it.
	if all, _ := s.AdminChatOverview(ctx, AdminChatFilter{IncludeArchived: true}); all.Chats != 5 {
		t.Fatalf("with archived = %d chats", all.Chats)
	}
	if li, _ := s.AdminChatOverview(ctx, AdminChatFilter{OwnerEmail: "LI@client.example"}); li.Chats != 1 || len(li.People) != 1 {
		t.Fatalf("one owner = %+v", li)
	}
}

// A transcript pages back from the latest entries with BeforeID.
func TestAdminChatTranscript_PagesBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	c, err := s.CreateConversation(ctx, "dana@client.example", "Long chat", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	var entries []agent.HistoryEntry
	for _, text := range []string{"one", "two", "three", "four", "five"} {
		entries = append(entries, entry(t, "user", "text", agent.TextContent{Text: text}))
	}
	if _, err := s.AppendHistory(ctx, c.ID, entries); err != nil {
		t.Fatal(err)
	}
	texts := func(es []AdminTranscriptEntry) string {
		out := make([]string, 0, len(es))
		for _, e := range es {
			out = append(out, e.Text)
		}
		return strings.Join(out, ",")
	}
	latest, err := s.AdminChatTranscript(ctx, c.ID, AdminTranscriptOptions{Tail: 2})
	if err != nil || texts(latest) != "four,five" {
		t.Fatalf("latest = %q, %v", texts(latest), err)
	}
	older, err := s.AdminChatTranscript(ctx, c.ID, AdminTranscriptOptions{Tail: 2, BeforeID: latest[0].ID})
	if err != nil || texts(older) != "two,three" {
		t.Fatalf("older = %q, %v", texts(older), err)
	}
	oldest, _ := s.AdminChatTranscript(ctx, c.ID, AdminTranscriptOptions{Tail: 2, BeforeID: older[0].ID})
	if texts(oldest) != "one" {
		t.Fatalf("oldest = %q", texts(oldest))
	}
}
