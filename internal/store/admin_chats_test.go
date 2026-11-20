package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func entry(t *testing.T, role, typ string, content any) agent.HistoryEntry {
	t.Helper()
	b, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return agent.HistoryEntry{Role: role, Type: typ, Content: b}
}

var cliAdmin = AdminActor{Email: "robbie@ops.example", Via: AdminViaCLI}

func TestAdminChats_ListGetAndFilters(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	a, err := s.CreateConversation(ctx, "a@client.example", "Opti Workbench", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateConversation(ctx, "b@client.example", "Pacing 50% check", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.CreateConversation(ctx, "a@client.example", "Deleted chat", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a@client.example", gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: a.ID, UserEmail: "a@client.example", Prompt: "sweep", CronExpr: "30 6 * * 1-5"}); err != nil {
		t.Fatal(err)
	}

	all, err := s.AdminListChats(ctx, AdminChatFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("list = %d, %v (deleted chats must not list)", len(all), err)
	}
	byOwner, _ := s.AdminListChats(ctx, AdminChatFilter{OwnerEmail: "A@client.example"})
	if len(byOwner) != 1 || byOwner[0].ID != a.ID || byOwner[0].ActivePatrols != 1 {
		t.Fatalf("by owner = %+v", byOwner)
	}
	// LIKE metacharacters in a search match literally.
	pct, _ := s.AdminListChats(ctx, AdminChatFilter{Search: "50%"})
	if len(pct) != 1 || pct[0].ID != b.ID {
		t.Fatalf("search 50%% = %+v", pct)
	}
	under, _ := s.AdminListChats(ctx, AdminChatFilter{Search: "_"})
	if len(under) != 0 {
		t.Fatalf("search _ matched %d chats", len(under))
	}

	got, err := s.AdminGetChat(ctx, a.ID[:MinChatScheduleIDPrefix])
	if err != nil || got.ID != a.ID || got.OwnerEmail != "a@client.example" || got.ApprovalMode != ConversationApprovalManual {
		t.Fatalf("get by prefix = %+v, %v", got, err)
	}
	if _, err := s.AdminGetChat(ctx, gone.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("deleted chat err = %v", err)
	}
	if _, err := s.AdminGetChat(ctx, "short"); err == nil {
		t.Fatal("a short prefix must be refused")
	}
}

func TestAdminUpdateChat(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	owner := "owner@client.example"
	c, err := s.CreateConversation(ctx, owner, "Old title", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	title, model, auto := "New title", "provider/m2", ConversationApprovalAuto
	if _, err := s.AdminUpdateChat(ctx, cliAdmin, c.ID, AdminChatChange{Title: &title}, " "); !errors.Is(err, ErrAdminReasonRequired) {
		t.Fatalf("no reason err = %v", err)
	}
	after, err := s.AdminUpdateChat(ctx, cliAdmin, c.ID, AdminChatChange{Title: &title, Model: &model, ApprovalMode: &auto}, "client asked")
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != title || after.Model != model || after.ApprovalMode != ConversationApprovalAuto {
		t.Fatalf("after = %+v", after)
	}
	autonomous := ConversationApprovalAutonomous
	if _, err := s.AdminUpdateChat(ctx, cliAdmin, c.ID, AdminChatChange{ApprovalMode: &autonomous}, "r"); !errors.Is(err, ErrAdminChatNotAllowed) {
		t.Fatalf("autonomous err = %v", err)
	}
	bad := "has space"
	if _, err := s.AdminUpdateChat(ctx, cliAdmin, c.ID, AdminChatChange{Model: &bad}, "r"); err == nil {
		t.Fatal("a model with a space must be refused")
	}
	yes := true
	if got, err := s.AdminUpdateChat(ctx, cliAdmin, c.ID, AdminChatChange{Archived: &yes}, "done"); err != nil || !got.Archived {
		t.Fatalf("archive = %+v, %v", got, err)
	}

	_, notices := chatNotices(t, s, c.ID)
	if len(notices) != 2 || notices[0].Kind != NoticeAdminUpdated || !strings.Contains(notices[0].Text, `renamed the chat to "New title"`) || !strings.Contains(notices[0].Text, "Reason: client asked") {
		t.Fatalf("notices = %+v", notices)
	}
	actions, err := s.ListAdminActions(ctx, c.ID, 10)
	if err != nil || len(actions) != 2 || actions[1].Actor != "robbie@ops.example" || actions[1].Via != AdminViaCLI || actions[1].Reason != "client asked" {
		t.Fatalf("actions = %+v, %v", actions, err)
	}

	lock, err := s.CreateConversation(ctx, owner, "Locked", "victoria", "m1", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdminUpdateChat(ctx, cliAdmin, lock.ID, AdminChatChange{Model: &model}, "r"); !errors.Is(err, ErrAdminChatNotAllowed) {
		t.Fatalf("lockdown model err = %v", err)
	}
}

func TestAdminRequests_MessageQueuesAsOwnerWithSender(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	owner := "owner@client.example"
	c, err := s.CreateConversation(ctx, owner, "Workbench", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: cliAdmin, Kind: AdminRequestMessage, ConversationID: c.ID, Payload: "hi"}); !errors.Is(err, ErrAdminReasonRequired) {
		t.Fatalf("no reason err = %v", err)
	}
	if _, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: cliAdmin, Kind: AdminRequestMessage, ConversationID: c.ID, Payload: " ", Reason: "r"}); err == nil {
		t.Fatal("an empty message must be refused")
	}
	if _, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: cliAdmin, Kind: "delete", ConversationID: c.ID, Reason: "r"}); err == nil {
		t.Fatal("an unknown kind must be refused")
	}

	req, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: cliAdmin, Kind: AdminRequestMessage, ConversationID: c.ID[:8], Payload: "Publish the team log now.", Reason: "set up the shared log"})
	if err != nil {
		t.Fatal(err)
	}
	if req.ConversationID != c.ID || req.State != AdminRequestPending {
		t.Fatalf("request = %+v", req)
	}
	pending, _ := s.ListPendingAdminRequests(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}

	row, ok, err := s.QueueAdminMessage(ctx, req.ID)
	if err != nil || !ok {
		t.Fatalf("queue = %v, %v", ok, err)
	}
	if row.Mode != InputModeAdmin || row.UserEmail != owner || row.SenderEmail != "robbie@ops.example" || row.State != InputStateQueued {
		t.Fatalf("row = %+v", row)
	}
	if !strings.HasPrefix(row.Message, "[Message from an administrator: robbie@ops.example]\nPublish the team log now.") || !strings.Contains(row.Message, "not by owner@client.example") {
		t.Fatalf("message = %q", row.Message)
	}
	// Delivered once: a second delivery is a no-op.
	if _, ok, err := s.QueueAdminMessage(ctx, req.ID); err != nil || ok {
		t.Fatalf("second delivery = %v, %v", ok, err)
	}
	got, err := s.GetAdminRequest(ctx, req.ID)
	if err != nil || got.State != AdminRequestDone || got.InputID != row.ID || got.InputState != InputStateQueued {
		t.Fatalf("request after = %+v, %v", got, err)
	}
	// The row round-trips with its sender, and the patrol sweep sees it.
	listed, _ := s.ListQueuedInputs(ctx, "", c.ID)
	if len(listed) != 1 || listed[0].SenderEmail != "robbie@ops.example" {
		t.Fatalf("listed = %+v", listed)
	}
	convs, _ := s.ListQueuedPatrolConversations(ctx)
	if len(convs) != 1 || convs[0] != c.ID {
		t.Fatalf("sweep = %v", convs)
	}
	actions, _ := s.ListAdminActions(ctx, c.ID, 10)
	if len(actions) != 1 || actions[0].Action != "request_message" {
		t.Fatalf("actions = %+v", actions)
	}
}

func TestAdminRequests_ArchivedChatAndSettling(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	owner := "owner@client.example"
	c, err := s.CreateConversation(ctx, owner, "Workbench", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	// An archived chat takes no admin message, and one archived after the
	// request was made fails at delivery.
	late, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: cliAdmin, Kind: AdminRequestMessage, ConversationID: c.ID, Payload: "x", Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetArchived(ctx, owner, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: cliAdmin, Kind: AdminRequestMessage, ConversationID: c.ID, Payload: "x", Reason: "r"}); !errors.Is(err, ErrAdminChatNotAllowed) {
		t.Fatalf("archived err = %v", err)
	}
	if _, ok, err := s.QueueAdminMessage(ctx, late.ID); err != nil || !ok {
		t.Fatalf("late delivery = %v, %v", ok, err)
	}
	if lr, _ := s.GetAdminRequest(ctx, late.ID); lr.State != AdminRequestFailed || lr.InputID != "" {
		t.Fatalf("late request = %+v", lr)
	}

	// Stop and transfer are settled by the server with FinishAdminRequest.
	stop, err := s.CreateAdminRequest(ctx, AdminRequestInput{Actor: cliAdmin, Kind: AdminRequestStop, ConversationID: c.ID, Reason: "runaway"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishAdminRequest(ctx, stop.ID, AdminRequestDone, "stopped"); err != nil || !ok {
		t.Fatalf("finish = %v, %v", ok, err)
	}
	if ok, _ := s.FinishAdminRequest(ctx, stop.ID, AdminRequestFailed, "again"); ok {
		t.Fatal("a settled request settled twice")
	}
}

func TestAdminChatTranscriptAndNotice(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	owner := "owner@client.example"
	c, err := s.CreateConversation(ctx, owner, "T", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
		entry(t, "user", "text", agent.TextContent{Text: "what did we do on Acme?"}),
		entry(t, "assistant", "reasoning", map[string]string{"text": "thinking"}),
		entry(t, "assistant", "tool_call", agent.ToolCallContent{ID: "1", Name: "run_python", Input: `{"code":"1"}`}),
		entry(t, "tool", "tool_result", agent.ToolResultContent{ID: "1", Name: "run_python", Text: "ok"}),
		entry(t, "assistant", "text", agent.TextContent{Text: strings.Repeat("x", 50)}),
		entry(t, "user", "text", agent.TextContent{Text: "[Message from an administrator: r@x]\nhello", Kind: agent.InputKindAdminMessage, SenderEmail: "r@x"}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AdminAppendNotice(ctx, cliAdmin, c.ID, "Heads up: the patrol changed."); err != nil {
		t.Fatal(err)
	}
	plain, err := s.AdminChatTranscript(ctx, c.ID, AdminTranscriptOptions{MaxTextRunes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 4 {
		t.Fatalf("plain transcript = %+v", plain)
	}
	if plain[1].Role != "assistant" || !plain[1].Truncated || plain[2].Kind != agent.InputKindAdminMessage || plain[2].Sender != "r@x" || plain[3].Type != agent.EntryTypeNotice {
		t.Fatalf("plain = %+v", plain)
	}
	tools, _ := s.AdminChatTranscript(ctx, c.ID, AdminTranscriptOptions{IncludeTools: true})
	if len(tools) != 6 || tools[1].Tool != "run_python" || tools[2].Text != "ok" {
		t.Fatalf("with tools = %+v", tools)
	}
	tail, _ := s.AdminChatTranscript(ctx, c.ID, AdminTranscriptOptions{Tail: 2})
	if len(tail) != 2 || tail[1].Type != agent.EntryTypeNotice {
		t.Fatalf("tail = %+v", tail)
	}
	if actions, _ := s.ListAdminActions(ctx, "", 10); len(actions) != 1 || actions[0].Action != "notice" {
		t.Fatalf("actions = %+v", actions)
	}
}

func TestAdminSetChatScheduleStatus(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	owner := "owner@client.example"
	c, err := s.CreateConversation(ctx, owner, "W", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateChatSchedule(ctx, ChatScheduleInput{ConversationID: c.ID, UserEmail: owner, Title: "Sweep", Prompt: "p", CronExpr: "30 6 * * 1-5"})
	if err != nil {
		t.Fatal(err)
	}
	paused, err := s.AdminSetChatScheduleStatus(ctx, cliAdmin, sc.ID, ChatScheduleStatusPaused, "holiday")
	if err != nil || paused.Status != ChatScheduleStatusPaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	resumed, err := s.AdminSetChatScheduleStatus(ctx, cliAdmin, sc.ID[:8], ChatScheduleStatusActive, "back")
	if err != nil || resumed.Status != ChatScheduleStatusActive {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	if _, err := s.AdminSetChatScheduleStatus(ctx, cliAdmin, sc.ID, "cancelled", "x"); err == nil {
		t.Fatal("only active/paused may be set")
	}
	actions, _ := s.ListAdminActions(ctx, c.ID, 10)
	if len(actions) != 2 || actions[0].Action != "patrol_resume" || actions[1].Action != "patrol_pause" || actions[1].ScheduleID != sc.ID {
		t.Fatalf("actions = %+v", actions)
	}
	if _, notices := chatNotices(t, s, c.ID); len(notices) != 2 || !strings.Contains(notices[0].Text, `paused the patrol "Sweep"`) {
		t.Fatalf("notices = %+v", notices)
	}

	// The operator prompt edit records its actor too.
	cur, _ := s.AdminGetChatSchedule(ctx, sc.ID)
	if _, _, err := s.AdminSetChatSchedulePrompt(ctx, AdminChatSchedulePromptEdit{ID: sc.ID, ExpectSHA256: cur.PromptSHA256, Prompt: "p2", Actor: &cliAdmin, Reason: "log moved"}); err != nil {
		t.Fatal(err)
	}
	actions, _ = s.ListAdminActions(ctx, c.ID, 1)
	if len(actions) != 1 || actions[0].Action != "patrol_prompt" || actions[0].Reason != "log moved" || !strings.Contains(actions[0].Detail, "->") {
		t.Fatalf("prompt action = %+v", actions)
	}
}

// One bad field changes nothing; an owner-locked title can still be renamed,
// and the administrator's title is locked against the auto-titler.
func TestAdminUpdateChat_ValidatesFirstAndRenamesLockedTitles(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	owner := "owner@client.example"
	c, err := s.CreateConversation(ctx, owner, "Auto title", "victoria", "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameTitle(ctx, owner, c.ID, "Owner's title"); err != nil {
		t.Fatal(err)
	}
	title, bad := "Admin title", "has space"
	if _, err := s.AdminUpdateChat(ctx, cliAdmin, c.ID, AdminChatChange{Title: &title, Model: &bad}, "r"); err == nil {
		t.Fatal("a bad model must refuse the whole change")
	}
	if got, _ := s.AdminGetChat(ctx, c.ID); got.Title != "Owner's title" {
		t.Fatalf("title changed by a refused request: %q", got.Title)
	}
	if acts, _ := s.ListAdminActions(ctx, c.ID, 10); len(acts) != 0 {
		t.Fatalf("a refused request was recorded: %+v", acts)
	}
	got, err := s.AdminUpdateChat(ctx, cliAdmin, c.ID, AdminChatChange{Title: &title}, "tidy")
	if err != nil || got.Title != title {
		t.Fatalf("rename of a locked title = %+v, %v", got, err)
	}
	if err := s.UpdateTitle(ctx, owner, c.ID, "auto-titler"); !errors.Is(err, ErrTitleLocked) {
		t.Fatalf("auto-titler after an admin rename err = %v, want ErrTitleLocked", err)
	}
}
