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
}

func (f *fakeChatsStore) AdminListChats(context.Context, store.AdminChatFilter) ([]store.AdminChat, error) {
	return []store.AdminChat{f.chat}, nil
}

func (f *fakeChatsStore) AdminGetChat(_ context.Context, id string) (store.AdminChat, error) {
	if !strings.HasPrefix(f.chat.ID, id) {
		return store.AdminChat{}, store.ErrConversationNotFound
	}
	return f.chat, nil
}

func (f *fakeChatsStore) AdminChatTranscript(context.Context, string, store.AdminTranscriptOptions) ([]store.AdminTranscriptEntry, error) {
	return []store.AdminTranscriptEntry{
		{Role: "user", Type: "text", Text: "hi"},
		{Role: "user", Type: "text", Kind: "admin_message", Sender: "robbie@ops", Text: "[Message from an administrator: robbie@ops]\npublish", CreatedAt: 100},
		{Role: "assistant", Type: "text", Text: f.reply, CreatedAt: 101},
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
