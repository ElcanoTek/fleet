package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

type fakeChatsStore struct {
	chat     store.AdminChat
	requests []store.AdminRequestInput
	updates  []store.AdminChatChange
	notices  []string
	actors   []store.AdminActor
	// polls drives GetAdminRequest: each call returns the next state.
	polls []store.AdminRequest
	pollN int
	reply string
	// filters and transcripts record what the listing, the overview and the
	// history were asked for.
	filters     []store.AdminChatFilter
	transcripts []store.AdminTranscriptOptions
	overview    store.AdminChatOverview
	note        *store.AdminNote
	cards       []store.AdminPendingCard
	// noteCalls records the note changes: "set:<text>" and "clear".
	noteCalls []string
}

func (f *fakeChatsStore) AdminListChats(_ context.Context, flt store.AdminChatFilter) ([]store.AdminChat, error) {
	f.filters = append(f.filters, flt)
	return []store.AdminChat{f.chat}, nil
}

func (f *fakeChatsStore) AdminChatOverview(_ context.Context, flt store.AdminChatFilter) (store.AdminChatOverview, error) {
	f.filters = append(f.filters, flt)
	return f.overview, nil
}

func (f *fakeChatsStore) AdminListPendingCards(context.Context, string) ([]store.AdminPendingCard, error) {
	return f.cards, nil
}

func (f *fakeChatsStore) GetAdminNote(context.Context, string) (*store.AdminNote, error) {
	return f.note, nil
}

func (f *fakeChatsStore) AdminSetNote(_ context.Context, a store.AdminActor, _ string, text string) (store.AdminNote, error) {
	f.actors = append(f.actors, a)
	f.noteCalls = append(f.noteCalls, "set:"+text)
	f.note = &store.AdminNote{Text: text, Author: a.Email, Via: a.Via, CreatedAt: 200, UpdatedAt: 200}
	return *f.note, nil
}

func (f *fakeChatsStore) AdminClearNote(_ context.Context, a store.AdminActor, _ string) (bool, error) {
	f.actors = append(f.actors, a)
	f.noteCalls = append(f.noteCalls, "clear")
	had := f.note != nil
	f.note = nil
	return had, nil
}

func (f *fakeChatsStore) AdminGetChat(_ context.Context, id string) (store.AdminChat, error) {
	if !strings.HasPrefix(f.chat.ID, id) {
		return store.AdminChat{}, store.ErrConversationNotFound
	}
	return f.chat, nil
}

func (f *fakeChatsStore) AdminChatTranscript(_ context.Context, _ string, o store.AdminTranscriptOptions) ([]store.AdminTranscriptEntry, error) {
	f.transcripts = append(f.transcripts, o)
	return []store.AdminTranscriptEntry{
		{ID: 41, Role: "user", Type: "text", Text: "hi"},
		{ID: 42, Role: "user", Type: "text", Kind: "admin_message", Sender: "robbie@ops", Text: "[Message from an administrator: robbie@ops]\npublish", CreatedAt: 100},
		{ID: 43, Role: "assistant", Type: "text", Text: f.reply, CreatedAt: 101},
	}, nil
}

func (f *fakeChatsStore) AdminAppendNotice(_ context.Context, a store.AdminActor, _ string, text string) error {
	f.actors = append(f.actors, a)
	f.notices = append(f.notices, text)
	return nil
}

func (f *fakeChatsStore) AdminUpdateChat(_ context.Context, a store.AdminActor, _ string, c store.AdminChatChange, _ string) (store.AdminChat, error) {
	f.actors = append(f.actors, a)
	f.updates = append(f.updates, c)
	return f.chat, nil
}

func (f *fakeChatsStore) ListAdminActions(context.Context, string, int) ([]store.AdminAction, error) {
	return []store.AdminAction{{Actor: "robbie@ops", Via: "cli", Action: "notice", ConversationID: f.chat.ID}}, nil
}

func (f *fakeChatsStore) CreateAdminRequest(_ context.Context, in store.AdminRequestInput) (store.AdminRequest, error) {
	f.requests = append(f.requests, in)
	return store.AdminRequest{ID: "req-1", Kind: in.Kind, ConversationID: f.chat.ID, State: store.AdminRequestPending, CreatedAt: 99}, nil
}

func (f *fakeChatsStore) GetAdminRequest(context.Context, string) (store.AdminRequest, error) {
	r := f.polls[min(f.pollN, len(f.polls)-1)]
	f.pollN++
	return r, nil
}

func (f *fakeChatsStore) AdminListChatSchedules(context.Context, store.AdminChatScheduleFilter) ([]store.AdminChatSchedule, error) {
	return nil, nil
}

func (f *fakeChatsStore) ListQueuedInputs(context.Context, string, string) ([]store.InputQueueRow, error) {
	return nil, nil
}

func withFakeChatsStore(t *testing.T) *fakeChatsStore {
	t.Helper()
	f := &fakeChatsStore{chat: store.AdminChat{ID: "dfe4adc7-6fd8-43cf-829c-0c723b2d42e6", Title: "Opti Workbench", OwnerEmail: "owner@client.example", ApprovalMode: "auto"}}
	prev, prevPoll := openChatsStore, waitPollInterval
	openChatsStore = func(string) (chatsStore, func(), int) { return f, func() {}, 0 }
	waitPollInterval = time.Millisecond
	t.Cleanup(func() { openChatsStore, waitPollInterval = prev, prevPoll })
	return f
}

func TestChatsSend_WaitsAndPrintsReply(t *testing.T) {
	f := withFakeChatsStore(t)
	f.reply = "Published 23 rows."
	base := store.AdminRequest{ID: "req-1", Kind: store.AdminRequestMessage, ConversationID: f.chat.ID, CreatedAt: 99}
	pending, queued, running, done := base, base, base, base
	pending.State = store.AdminRequestPending
	queued.State, queued.InputState = store.AdminRequestDone, store.InputStateQueued
	running.State, running.InputState = store.AdminRequestDone, store.InputStateRunning
	done.State, done.InputState = store.AdminRequestDone, store.InputStateCompleted
	f.polls = []store.AdminRequest{pending, queued, running, done}

	var out, errW bytes.Buffer
	code := chatsRequest(store.AdminRequestMessage, []string{"dfe4adc7", "--message", "Publish the team log.", "--reason", "seed", "--as", "Robbie@Ops", "--wait"}, strings.NewReader(""), &out, &errW)
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out.String(), errW.String())
	}
	if len(f.requests) != 1 {
		t.Fatalf("requests = %+v", f.requests)
	}
	r := f.requests[0]
	if r.Kind != store.AdminRequestMessage || r.Payload != "Publish the team log." || r.Reason != "seed" || r.Actor.Email != "robbie@ops" || r.Actor.Via != store.AdminViaCLI {
		t.Fatalf("request = %+v", r)
	}
	for _, want := range []string{"the turn is running", "the turn ended (completed)"} {
		if !strings.Contains(errW.String(), want) {
			t.Fatalf("progress missing %q:\n%s", want, errW.String())
		}
	}
	if !strings.Contains(out.String(), "reply:\nPublished 23 rows.") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestChatsRequest_RefusalsAndFailure(t *testing.T) {
	f := withFakeChatsStore(t)
	run := func(kind string, args ...string) int {
		return chatsRequest(kind, args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	}
	if run(store.AdminRequestMessage, "dfe4adc7", "--reason", "r") == 0 {
		t.Fatal("a message without text must be refused")
	}
	if run(store.AdminRequestMessage, "dfe4adc7", "--message", "x") == 0 {
		t.Fatal("a message without a reason must be refused")
	}
	if run(store.AdminRequestTransfer, "dfe4adc7", "--reason", "r") == 0 {
		t.Fatal("a transfer without --to must be refused")
	}
	if len(f.requests) != 0 {
		t.Fatalf("a refused request was recorded: %+v", f.requests)
	}
	f.polls = []store.AdminRequest{{ID: "req-1", Kind: store.AdminRequestTransfer, State: store.AdminRequestFailed, Result: "a turn is running"}}
	if run(store.AdminRequestTransfer, "dfe4adc7", "--to", "new@client.example", "--reason", "r", "--wait") == 0 {
		t.Fatal("a failed request must exit non-zero")
	}
	if len(f.requests) != 1 || f.requests[0].Payload != "new@client.example" {
		t.Fatalf("transfer request = %+v", f.requests)
	}
}

func TestChatsSetNoticeShowHistory(t *testing.T) {
	f := withFakeChatsStore(t)
	var out bytes.Buffer
	if code := chatsSet([]string{"dfe4adc7", "--approval-mode", "manual", "--archive", "--reason", "client left", "--as", "ops@elcano.example"}, &out); code != 0 {
		t.Fatalf("set code %d", code)
	}
	if len(f.updates) != 1 || f.updates[0].ApprovalMode == nil || *f.updates[0].ApprovalMode != "manual" || f.updates[0].Title != nil {
		t.Fatalf("update = %+v", f.updates)
	}
	// Archiving goes to the server as a request (it revokes participants).
	if len(f.requests) != 1 || f.requests[0].Kind != store.AdminRequestArchive || f.requests[0].Payload != "archive" {
		t.Fatalf("archive request = %+v", f.requests)
	}
	if f.actors[0].Email != "ops@elcano.example" {
		t.Fatalf("actor = %+v (--as)", f.actors[0])
	}
	if code := chatsSet([]string{"dfe4adc7", "--reason", "r"}, &out); code == 0 {
		t.Fatal("set with nothing to change must be refused")
	}
	if code := chatsSet([]string{"dfe4adc7", "--title", "x"}, &out); code == 0 {
		t.Fatal("set without a reason must be refused")
	}
	if code := chatsNotice([]string{"dfe4adc7", "--text", "Heads up"}, &out); code != 0 || len(f.notices) != 1 {
		t.Fatalf("notice code %d %v", code, f.notices)
	}
	out.Reset()
	if code := chatsShow([]string{"dfe4adc7"}, &out); code != 0 || !strings.Contains(out.String(), "owner:       owner@client.example") || !strings.Contains(out.String(), "recent admin changes") {
		t.Fatalf("show = %d\n%s", code, out.String())
	}
	out.Reset()
	if code := chatsHistory([]string{"dfe4adc7", "--tail", "5"}, &out); code != 0 || !strings.Contains(out.String(), "admin message from robbie@ops") || !strings.Contains(out.String(), "user owner@client.example") {
		t.Fatalf("history = %d\n%s", code, out.String())
	}
	out.Reset()
	if code := chatsList([]string{"--json"}, &out); code != 0 {
		t.Fatalf("list code %d", code)
	}
	var list []store.AdminChat
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("list json = %v %s", err, out.String())
	}
}

func TestCLIAdminActorDefault(t *testing.T) {
	a := cliAdminActor("")
	if a.Via != store.AdminViaCLI || !strings.Contains(a.Email, "@") {
		t.Fatalf("default actor = %+v", a)
	}
	if got := cliAdminActor(" Robbie@Elcano.example "); got.Email != "robbie@elcano.example" {
		t.Fatalf("--as actor = %+v", got)
	}
}

func TestChatsDelete_RecycleBinAndPermanent(t *testing.T) {
	f := withFakeChatsStore(t)
	f.polls = []store.AdminRequest{{ID: "req-1", State: store.AdminRequestPending}}
	run := func(args ...string) int {
		return chatsRequest(store.AdminRequestDelete, args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	}
	if run("dfe4adc7", "--reason", "forked by mistake") != 0 || f.requests[0].Kind != store.AdminRequestDelete {
		t.Fatalf("delete = %+v", f.requests)
	}
	if run("dfe4adc7", "--permanent", "--reason", "disk full") == 0 {
		t.Fatal("--permanent without --yes must be refused")
	}
	if run("dfe4adc7", "--permanent", "--yes", "--reason", "disk full") != 0 || f.requests[len(f.requests)-1].Kind != store.AdminRequestPurge {
		t.Fatalf("permanent delete = %+v", f.requests)
	}
	if len(f.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(f.requests))
	}
}

func TestChatsList_StateFilterAndFlags(t *testing.T) {
	f := withFakeChatsStore(t)
	f.chat.Attention = []string{store.AdminAttentionCardsWaiting, store.AdminAttentionTurnFailed}
	f.chat.PendingApprovals, f.chat.HasNote = 1, true
	var out bytes.Buffer
	if code := chatsList([]string{"--state", "noted", "--search", "owner@"}, &out); code != 0 {
		t.Fatalf("list --state code %d", code)
	}
	if len(f.filters) != 1 || f.filters[0].State != store.AdminChatStateNoted || f.filters[0].Search != "owner@" {
		t.Fatalf("filter = %+v", f.filters)
	}
	for _, want := range []string{"attention:cards_waiting+turn_failed", "cards:1", " note"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "note:seen") {
		t.Fatalf("an unseen note is listed as seen:\n%s", out.String())
	}
	f.chat.NoteSeen = true
	out.Reset()
	// --attention is --state attention; naming the same state twice is fine.
	if code := chatsList([]string{"--attention", "--state", "attention"}, &out); code != 0 || f.filters[1].State != store.AdminChatStateAttention {
		t.Fatalf("list --attention = %d %+v", code, f.filters)
	}
	if !strings.Contains(out.String(), "note:seen") {
		t.Fatalf("list missing note:seen:\n%s", out.String())
	}
	if code := chatsList([]string{"--attention"}, &out); code != 0 || f.filters[2].State != store.AdminChatStateAttention {
		t.Fatalf("list --attention = %d %+v", code, f.filters)
	}
	// Refused before the store is opened.
	if code := chatsList([]string{"--state", "stuck"}, &out); code == 0 {
		t.Fatal("an unknown --state must be refused")
	}
	if code := chatsList([]string{"--attention", "--state", "running"}, &out); code == 0 {
		t.Fatal("--attention with another --state must be refused")
	}
	if len(f.filters) != 3 {
		t.Fatalf("a refused listing reached the store: %+v", f.filters)
	}
}

func TestChatsPeople(t *testing.T) {
	f := withFakeChatsStore(t)
	f.overview = store.AdminChatOverview{
		AdminChatCounts: store.AdminChatCounts{Chats: 7, Attention: 2, Running: 1, Patrols: 3, Noted: 1, CardsWaiting: 4},
		Archived:        5, Deleted: 6,
		People: []store.AdminChatPerson{
			{Email: "stuck@client.example", AdminChatCounts: store.AdminChatCounts{Chats: 4, Attention: 2, CardsWaiting: 4, Noted: 1}, LastActive: 1700000000},
			{Email: "fine@client.example", AdminChatCounts: store.AdminChatCounts{Chats: 3, Running: 1, Patrols: 3}, LastActive: 1700003600},
		},
	}
	var out bytes.Buffer
	if code := chatsPeople([]string{"--user", "stuck@client.example", "--active-days", "7", "--archived"}, &out); code != 0 {
		t.Fatalf("people code %d", code)
	}
	if flt := f.filters[0]; flt.OwnerEmail != "stuck@client.example" || flt.ActiveWithinDays != 7 || !flt.IncludeArchived {
		t.Fatalf("filter = %+v", flt)
	}
	got := out.String()
	for _, want := range []string{
		"PERSON", "LAST ACTIVE (UTC)", "stuck@client.example", "2023-11-14 22:13",
		"7 chat(s): 2 need attention, 4 approval card(s) waiting, 1 running, 3 with patrols, 1 with a note; 5 archived, 6 in the recycle bin",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("people missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "stuck@") > strings.Index(got, "fine@") {
		t.Fatalf("people are not in the store's order:\n%s", got)
	}
	out.Reset()
	if code := chatsPeople([]string{"--json"}, &out); code != 0 {
		t.Fatalf("people --json code %d", code)
	}
	var o store.AdminChatOverview
	if err := json.Unmarshal(out.Bytes(), &o); err != nil || o.Chats != 7 || o.Deleted != 6 || len(o.People) != 2 || o.People[0].CardsWaiting != 4 {
		t.Fatalf("people json = %v %+v", err, o)
	}
	if code := chatsPeople([]string{"extra"}, &out); code == 0 {
		t.Fatal("people takes no positional argument")
	}
}

func TestChatsNote(t *testing.T) {
	f := withFakeChatsStore(t)
	note := func(stdin string, args ...string) (int, string) {
		var out bytes.Buffer
		code := chatsNote(args, strings.NewReader(stdin), &out)
		return code, out.String()
	}
	// Show: none yet.
	if code, got := note("", "dfe4adc7"); code != 0 || !strings.Contains(got, "no note is pinned to") {
		t.Fatalf("show (none) = %d\n%s", code, got)
	}
	if code, got := note("", "dfe4adc7", "--json"); code != 0 || !strings.Contains(got, `"note": null`) {
		t.Fatalf("show --json (none) = %d\n%s", code, got)
	}
	// Set from --text.
	code, got := note("", "dfe4adc7", "--text", " Your card is waiting. ", "--as", "Robbie@Ops")
	if code != 0 || !strings.Contains(got, "see it above the conversation") || !strings.Contains(got, "the assistant never reads it") {
		t.Fatalf("set = %d\n%s", code, got)
	}
	if len(f.noteCalls) != 1 || f.noteCalls[0] != "set:Your card is waiting." || f.actors[0].Email != "robbie@ops" || f.actors[0].Via != store.AdminViaCLI {
		t.Fatalf("set calls = %v actors = %+v", f.noteCalls, f.actors)
	}
	// Replace from stdin, as JSON.
	code, got = note("Line one\nLine two\n", "dfe4adc7", "--file", "-", "--json")
	var set struct {
		Note *store.AdminNote `json:"note"`
	}
	if err := json.Unmarshal([]byte(got), &set); code != 0 || err != nil || set.Note == nil || set.Note.Text != "Line one\nLine two" {
		t.Fatalf("set --file - --json = %d %v\n%s", code, err, got)
	}
	// Show: the text, its author and the seen state.
	if code, got = note("", "dfe4adc7"); code != 0 || !strings.Contains(got, "  Line two") || !strings.Contains(got, "not seen yet") || !strings.Contains(got, "via cli") {
		t.Fatalf("show = %d\n%s", code, got)
	}
	seen := int64(1700000000)
	f.note.SeenAt, f.note.SeenBy = &seen, "owner@client.example"
	if code, got = note("", "dfe4adc7"); code != 0 || !strings.Contains(got, "seen by owner@client.example 2023-11-14 22:13 UTC") {
		t.Fatalf("show (seen) = %d\n%s", code, got)
	}
	// Refusals never reach the store.
	calls := len(f.noteCalls)
	for _, args := range [][]string{
		{"dfe4adc7", "--text", "x", "--clear"},
		{"dfe4adc7", "--text", "x", "--file", "-"},
		{"dfe4adc7", "--file", "-", "--clear"},
		{"dfe4adc7", "--text", "  "},
		{"dfe4adc7", "--text", strings.Repeat("é", store.MaxAdminNoteRunes+1)},
		{"--text", "x"},
	} {
		if code, _ := note("from stdin", args...); code == 0 {
			t.Fatalf("note %v must be refused", args[:min(len(args), 2)])
		}
	}
	if len(f.noteCalls) != calls {
		t.Fatalf("a refused note reached the store: %v", f.noteCalls)
	}
	// A note of exactly the limit, counted in characters not bytes, is taken.
	if code, _ = note("", "dfe4adc7", "--text", strings.Repeat("é", store.MaxAdminNoteRunes)); code != 0 {
		t.Fatalf("a %d-character note was refused", store.MaxAdminNoteRunes)
	}
	// Clear, then clear again.
	if code, got = note("", "dfe4adc7", "--clear"); code != 0 || !strings.Contains(got, "note removed from") {
		t.Fatalf("clear = %d\n%s", code, got)
	}
	if code, got = note("", "dfe4adc7", "--clear"); code != 0 || !strings.Contains(got, "no note was pinned to") {
		t.Fatalf("clear (none) = %d\n%s", code, got)
	}
	f.note = &store.AdminNote{Text: "x"}
	if code, got = note("", "dfe4adc7", "--clear", "--json"); code != 0 || !strings.Contains(got, `"cleared": true`) {
		t.Fatalf("clear --json = %d\n%s", code, got)
	}
	if code, got = note("", "dfe4adc7", "--clear", "--json"); code != 0 || !strings.Contains(got, `"cleared": false`) {
		t.Fatalf("clear --json (none) = %d\n%s", code, got)
	}
}

func TestChatsShow_AttentionNoteAndCards(t *testing.T) {
	f := withFakeChatsStore(t)
	f.chat.PendingApprovals, f.chat.WaitingSince, f.chat.LastTurnStatus = 1, 1700000000, "error"
	f.chat.Attention = []string{store.AdminAttentionCardsWaiting, store.AdminAttentionTurnFailed}
	f.note = &store.AdminNote{Text: "Approve the card\nwhen you are back.", Author: "robbie@ops", Via: store.AdminViaCLI, UpdatedAt: 1700000100}
	f.cards = []store.AdminPendingCard{
		{ID: "c1", Tool: "send_email", MCPServer: "ms365", ForEmail: "owner@client.example", CreatedAt: 1700000000, ExpiresAt: 1700003600},
		{ID: "c2", Tool: "bash", ForEmail: "guest@client.example", CreatedAt: 1700000060},
	}
	var out bytes.Buffer
	if code := chatsShow([]string{"dfe4adc7"}, &out); code != 0 {
		t.Fatalf("show code %d", code)
	}
	for _, want := range []string{
		"attention:   an approval card is waiting since 2023-11-14 22:13 UTC; the latest turn ended in an error",
		"pinned note (by robbie@ops via cli, 2023-11-14 22:15 UTC; not seen yet):",
		"  Approve the card\n  when you are back.",
		"waiting approval cards",
		"ms365/send_email",
		"for owner@client.example, waiting since 2023-11-14 22:13 UTC, expires 2023-11-14 23:13 UTC",
		"for guest@client.example, waiting since 2023-11-14 22:14 UTC, does not expire",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("show missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := chatsShow([]string{"dfe4adc7", "--json"}, &out); code != 0 {
		t.Fatalf("show --json code %d", code)
	}
	var got struct {
		Chat         store.AdminChat          `json:"chat"`
		Note         *store.AdminNote         `json:"note"`
		PendingCards []store.AdminPendingCard `json:"pending_cards"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Note == nil || got.Note.Author != "robbie@ops" || len(got.PendingCards) != 2 || len(got.Chat.Attention) != 2 {
		t.Fatalf("show json = %v\n%s", err, out.String())
	}
	// The card list is bounded and the count is not: the difference is said.
	f.chat.PendingApprovals = 60
	out.Reset()
	if code := chatsShow([]string{"dfe4adc7"}, &out); code != 0 || !strings.Contains(out.String(), "these are the 2 that have waited longest; 58 more are waiting") {
		t.Fatalf("show with more cards than listed = %d\n%s", code, out.String())
	}
	// A quiet chat prints none of it, and its JSON note is null.
	f.chat.Attention, f.note, f.cards = nil, nil, nil
	out.Reset()
	if code := chatsShow([]string{"dfe4adc7"}, &out); code != 0 || strings.Contains(out.String(), "attention:") || strings.Contains(out.String(), "pinned note") || strings.Contains(out.String(), "waiting approval cards") {
		t.Fatalf("quiet show = %d\n%s", code, out.String())
	}
	out.Reset()
	if code := chatsShow([]string{"dfe4adc7", "--json"}, &out); code != 0 || !strings.Contains(out.String(), `"note": null`) {
		t.Fatalf("quiet show --json = %d\n%s", code, out.String())
	}
}

func TestChatsHistory_BeforeAndOlderHint(t *testing.T) {
	f := withFakeChatsStore(t)
	var out bytes.Buffer
	// The fake returns three entries: a page of three is full.
	if code := chatsHistory([]string{"dfe4adc7", "--tail", "3", "--before", "90"}, &out); code != 0 {
		t.Fatalf("history code %d", code)
	}
	if o := f.transcripts[0]; o.BeforeID != 90 || o.Tail != 3 {
		t.Fatalf("transcript options = %+v", o)
	}
	// The hint continues the same reading: the page size that was asked for.
	if want := "older entries: fleet chats history " + f.chat.ID + " --before 41 --tail 3\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("history does not end with %q:\n%s", want, out.String())
	}
	// And the tool calls, when they were asked for.
	out.Reset()
	if code := chatsHistory([]string{"dfe4adc7", "--tail", "3", "--tools"}, &out); code != 0 {
		t.Fatalf("history --tools code %d", code)
	}
	if want := " --before 41 --tools --tail 3\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("history --tools does not end with %q:\n%s", want, out.String())
	}
	f.transcripts = f.transcripts[:1]
	// A page that is not full is the last one: no hint. Nor in JSON.
	out.Reset()
	if code := chatsHistory([]string{"dfe4adc7", "--tail", "5"}, &out); code != 0 || strings.Contains(out.String(), "older entries") {
		t.Fatalf("short page = %d\n%s", code, out.String())
	}
	if f.transcripts[1].BeforeID != 0 {
		t.Fatalf("transcript options = %+v", f.transcripts[1])
	}
	out.Reset()
	if code := chatsHistory([]string{"dfe4adc7", "--tail", "3", "--json"}, &out); code != 0 || strings.Contains(out.String(), "older entries") {
		t.Fatalf("history --json = %d\n%s", code, out.String())
	}
	if code := chatsHistory([]string{"dfe4adc7", "--before", "-1"}, &out); code == 0 {
		t.Fatal("a negative --before must be refused")
	}
}

func TestChatsNotice_JSON(t *testing.T) {
	f := withFakeChatsStore(t)
	var out bytes.Buffer
	if code := chatsNotice([]string{"dfe4adc7", "--text", "Heads up", "--json"}, &out); code != 0 || len(f.notices) != 1 {
		t.Fatalf("notice --json = %d %v", code, f.notices)
	}
	var got struct {
		OK     bool   `json:"ok"`
		ChatID string `json:"chat_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || !got.OK || got.ChatID != f.chat.ID {
		t.Fatalf("notice json = %v\n%s", err, out.String())
	}
}
