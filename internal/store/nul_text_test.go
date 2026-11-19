package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// Free-form content holding U+0000 — a user message, a model's answer, a
// title, a memory, a persona's patrols (jsonb) — is written without the
// character (the store's content writers remove it, internal/pgtext). With
// search on (the default) a turn's history commit also writes the decoded
// text to message_search_content in the same transaction, and that copy is
// stored without it too, so the commit goes through. Keys are stored as given,
// so a key holding U+0000 still fails its write.
func TestTextWithNULIsStoredWithoutIt(t *testing.T) {
	t.Parallel()
	f := &nulFixture{s: newTestStore(t), ctx: context.Background()}
	if !f.s.searchEnabled {
		t.Fatal("search must be on, as by default")
	}
	for _, c := range []struct {
		name string
		run  func(*testing.T, *nulFixture)
	}{
		{"conversation create", nulConversationCreate},
		{"conversation rename", nulConversationRename},
		{"user message commit", nulUserMessageCommit},
		{"turn history commit", nulTurnHistoryCommit},
		{"keys are stored as given", nulKeysKeepFailing},
		{"webhook conversation", nulWebhookConversation},
		{"webhook input commit", nulWebhookInputCommit},
		{"input queue", nulInputQueue},
		{"memory create", nulMemoryCreate},
		{"memory update", nulMemoryUpdate},
		{"memory proposal", nulMemoryProposal},
		{"project create", nulProjectCreate},
		{"project update", nulProjectUpdate},
		{"project memory create and update", nulProjectMemory},
		{"persona create", nulPersonaCreate},
		{"persona update of the jsonb patrols", nulPersonaUpdate},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, f) })
	}
}

const nulUser = "u@example.com"

// nulFixture is the one store the NUL subtests share, with helpers that set
// up their clean preconditions.
type nulFixture struct {
	s     *Store
	ctx   context.Context
	turns int
}

func expectNoNUL(t *testing.T, what, got, want string) {
	t.Helper()
	if strings.ContainsRune(got, 0) || got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func (f *nulFixture) conv(t *testing.T) *Conversation {
	t.Helper()
	conv, err := f.s.CreateConversation(f.ctx, nulUser, "clean", "", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv
}

func (f *nulFixture) title(t *testing.T, convID string) string {
	t.Helper()
	got, err := f.s.Get(f.ctx, nulUser, convID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	return got.Title
}

func (f *nulFixture) turn(t *testing.T, convID string) string {
	t.Helper()
	f.turns++
	turnID := fmt.Sprintf("turn-%d", f.turns)
	if err := f.s.CreateTurn(f.ctx, turnID, convID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return turnID
}

func (f *nulFixture) searchContent(t *testing.T, messageID int64) string {
	t.Helper()
	var got string
	if err := f.s.db.QueryRowContext(f.ctx,
		`SELECT content FROM message_search_content WHERE message_id = $1`, messageID).Scan(&got); err != nil {
		t.Fatalf("search row for message %d: %v", messageID, err)
	}
	return got
}

func (f *nulFixture) project(t *testing.T) *Project {
	t.Helper()
	proj, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: nulUser, Name: "Proj", TeamID: "t"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return proj
}

func (f *nulFixture) webhookConversation(t *testing.T, key, title string) (*Conversation, string) {
	t.Helper()
	claim, err := f.s.ClaimWebhookDelivery(f.ctx, "gh", key)
	if err != nil || !claim.Claimed {
		t.Fatalf("ClaimWebhookDelivery = %+v, %v", claim, err)
	}
	hook, err := f.s.CreateWebhookConversation(f.ctx, "gh", key, claim.Token, nulUser, title, "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateWebhookConversation: %v", err)
	}
	return hook, claim.Token
}

func nulConversationCreate(t *testing.T, f *nulFixture) {
	conv, err := f.s.CreateConversation(f.ctx, nulUser, "Q3\x00 plan", "", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	expectNoNUL(t, "title", f.title(t, conv.ID), "Q3 plan")
}

func nulConversationRename(t *testing.T, f *nulFixture) {
	conv := f.conv(t)
	if err := f.s.UpdateTitle(f.ctx, nulUser, conv.ID, "renamed\x00"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	expectNoNUL(t, "title", f.title(t, conv.ID), "renamed")
}

func nulUserMessageCommit(t *testing.T, f *nulFixture) {
	conv := f.conv(t)
	turnID := f.turn(t, conv.ID)
	id, err := f.s.CommitUserMessage(f.ctx, conv.ID, turnID, textEntry("user", "find\x00 me"))
	if err != nil {
		t.Fatalf("CommitUserMessage: %v", err)
	}
	if got := loadEntries(t, f.s, conv.ID); len(got) != 1 {
		t.Fatalf("history rows = %d, want 1", len(got))
	}
	expectNoNUL(t, "search content", f.searchContent(t, id), "find me")
}

func nulTurnHistoryCommit(t *testing.T, f *nulFixture) {
	conv := f.conv(t)
	turnID := f.turn(t, conv.ID)
	if _, err := f.s.CommitUserMessage(f.ctx, conv.ID, turnID, textEntry("user", "q")); err != nil {
		t.Fatal(err)
	}
	ids, err := f.s.CommitTurnHistory(f.ctx, conv.ID, turnID, []agent.HistoryEntry{textEntry("assistant", "the\x00 answer")})
	if err != nil {
		t.Fatalf("CommitTurnHistory: %v", err)
	}
	if got := loadEntries(t, f.s, conv.ID); len(got) != 2 {
		t.Fatalf("history rows = %d, want 2", len(got))
	}
	expectNoNUL(t, "search content", f.searchContent(t, ids[0]), "the answer")
	res, _, err := f.s.SearchConversations(f.ctx, nulUser, "answer\x00", 10, 0)
	if err != nil || len(res) != 1 || res[0].ConversationID != conv.ID {
		t.Fatalf("SearchConversations = %+v, %v; want this conversation", res, err)
	}
}

// Keys are stored as given, so a key holding U+0000 fails its write just as a
// lookup with it fails: the input queue's client key, a project's MCP server
// name (a catalog key), a conversation's owner.
func nulKeysKeepFailing(t *testing.T, f *nulFixture) {
	conv := f.conv(t)
	if _, _, err := f.s.EnqueueInput(f.ctx, InputQueueRow{
		ID: "iq-key", ConversationID: conv.ID, UserEmail: nulUser,
		ClientInputID: "cli\x00", Message: "m", Attachments: "[]", Mode: InputModeQueued,
	}); err == nil {
		t.Error("EnqueueInput with a client key holding U+0000 succeeded")
	}
	if _, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: nulUser, Name: "P", TeamID: "t", MCPServers: []string{"srv\x00"}}); err == nil {
		t.Error("CreateProject with an MCP server name holding U+0000 succeeded")
	}
	if _, err := f.s.CreateConversation(f.ctx, nulUser+"\x00", "t", "", "m", false); err == nil {
		t.Error("CreateConversation for an owner holding U+0000 succeeded")
	}
}

func nulWebhookConversation(t *testing.T, f *nulFixture) {
	hook, _ := f.webhookConversation(t, "github:title", "hook\x00")
	expectNoNUL(t, "webhook title", f.title(t, hook.ID), "hook")
}

// A webhook delivery's input commit, which every redelivery retries, stores
// the payload text without the character.
func nulWebhookInputCommit(t *testing.T, f *nulFixture) {
	hook, token := f.webhookConversation(t, "github:input", "hook")
	turnID := f.turn(t, hook.ID)
	if _, err := f.s.CommitWebhookUserMessage(f.ctx, "gh", "github:input", token, hook.ID, turnID, textEntry("user", "payload\x00")); err != nil {
		t.Fatalf("CommitWebhookUserMessage: %v", err)
	}
}

func nulInputQueue(t *testing.T, f *nulFixture) {
	conv := f.conv(t)
	if _, _, err := f.s.EnqueueInput(f.ctx, InputQueueRow{
		ID: "iq-nul", ConversationID: conv.ID, UserEmail: nulUser,
		ClientInputID: "cli-nul", Message: "queued\x00", Attachments: "[]", Mode: InputModeQueued,
	}); err != nil {
		t.Fatalf("EnqueueInput: %v", err)
	}
	items, err := f.s.ListQueuedInputs(f.ctx, nulUser, conv.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListQueuedInputs = %+v, %v", items, err)
	}
	expectNoNUL(t, "queued message", items[0].Message, "queued")
}

func nulMemoryCreate(t *testing.T, f *nulFixture) {
	mem, err := f.s.CreateMemory(f.ctx, nulUser, "likes\x00 tea", "manual", "preference")
	if err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	expectNoNUL(t, "memory", mem.Content, "likes tea")
}

func nulMemoryUpdate(t *testing.T, f *nulFixture) {
	mem, err := f.s.CreateMemory(f.ctx, nulUser, "likes milk", "manual", "preference")
	if err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	coffee := "likes\x00 coffee"
	if mem, err = f.s.UpdateMemory(f.ctx, nulUser, mem.ID, MemoryPatch{Content: &coffee}); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	expectNoNUL(t, "memory", mem.Content, "likes coffee")
}

func nulMemoryProposal(t *testing.T, f *nulFixture) {
	conv := f.conv(t)
	prop, err := f.s.CreateMemoryProposal(f.ctx, nulUser, conv.ID, MemoryProposalParams{Content: "proposed\x00", Kind: "fact", Origin: "tool"})
	if err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	expectNoNUL(t, "proposed memory", prop.Content, "proposed")
}

func expectProject(t *testing.T, got *Project, name, instructions string) {
	t.Helper()
	expectNoNUL(t, "project name", got.Name, name)
	expectNoNUL(t, "project instructions", got.Instructions, instructions)
}

func nulProjectCreate(t *testing.T, f *nulFixture) {
	proj, err := f.s.CreateProject(f.ctx, &Project{OwnerEmail: nulUser, Name: "Proj\x00", Instructions: "do\x00 it", TeamID: "t"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	got, err := f.s.GetProject(f.ctx, proj.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	expectProject(t, got, "Proj", "do it")
}

func nulProjectUpdate(t *testing.T, f *nulFixture) {
	proj := f.project(t)
	name, instr := "Renamed\x00", "new\x00 rules"
	got, err := f.s.UpdateProject(f.ctx, nulUser, proj.ID, ProjectPatch{Name: &name, Instructions: &instr})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	expectProject(t, got, "Renamed", "new rules")
}

func nulProjectMemory(t *testing.T, f *nulFixture) {
	proj := f.project(t)
	pm, err := f.s.CreateProjectMemory(f.ctx, proj.ID, nulUser, "shared\x00 fact", "note")
	if err != nil {
		t.Fatalf("CreateProjectMemory: %v", err)
	}
	expectNoNUL(t, "project memory", pm.Content, "shared fact")
	edited := "edited\x00"
	if pm, err = f.s.UpdateProjectMemory(f.ctx, proj.ID, pm.ID, nulUser, MemoryPatch{Content: &edited}); err != nil {
		t.Fatalf("UpdateProjectMemory: %v", err)
	}
	expectNoNUL(t, "project memory", pm.Content, "edited")
}

// A persona's instructions are text, its patrols jsonb.
func nulPersonaCreate(t *testing.T, f *nulFixture) {
	in := validPersonaInput("nul-create")
	in.Instructions = "Lead\x00 with the number."
	in.Patrols[0].Title = "Morning\x00 sweep"
	in.Patrols[0].Prompt = "Check\x00 pacing."
	p, err := f.s.CreateUserPersona(f.ctx, nulUser, in)
	if err != nil {
		t.Fatalf("CreateUserPersona: %v", err)
	}
	expectNoNUL(t, "persona instructions", p.Instructions, "Lead with the number.")
	if len(p.Patrols) != 1 {
		t.Fatalf("persona patrols = %+v", p.Patrols)
	}
	expectNoNUL(t, "patrol title", p.Patrols[0].Title, "Morning sweep")
	expectNoNUL(t, "patrol prompt", p.Patrols[0].Prompt, "Check pacing.")
}

func nulPersonaUpdate(t *testing.T, f *nulFixture) {
	in := validPersonaInput("nul-update")
	p, err := f.s.CreateUserPersona(f.ctx, nulUser, in)
	if err != nil {
		t.Fatalf("CreateUserPersona: %v", err)
	}
	in.Patrols[0].Title = "Evening\x00 sweep"
	if p, err = f.s.UpdateUserPersona(f.ctx, nulUser, p.ID, in); err != nil {
		t.Fatalf("UpdateUserPersona: %v", err)
	}
	if len(p.Patrols) != 1 {
		t.Fatalf("persona patrols = %+v", p.Patrols)
	}
	expectNoNUL(t, "patrol title", p.Patrols[0].Title, "Evening sweep")
}

// A turn stranded by a crash whose streamed answer holds U+0000 is recovered
// at boot: the projection writes the decoded text to message_search_content in
// the recovery transaction without the character, and the turn ends 'error'
// with its answer in history.
func TestRecovery_StrandedTurnWithNULText(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	convID := seedConvAndTurn(t, s, "t1")
	if _, err := s.CommitUserMessage(ctx, convID, "t1", userEntry(t, "q")); err != nil {
		t.Fatal(err)
	}
	seedEvent(t, s, "t1", 1, "text.delta", map[string]any{"text": "partial\x00 answer"})

	recs, err := s.RecoverStrandedTurns(ctx)
	if err != nil || len(recs) != 1 || recs[0].Projected == 0 {
		t.Fatalf("RecoverStrandedTurns = %+v, %v; want the turn recovered", recs, err)
	}
	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT content FROM message_search_content WHERE conversation_id = $1 AND content LIKE 'partial%'`,
		convID).Scan(&got); err != nil {
		t.Fatalf("search row: %v", err)
	}
	if got != "partial answer" {
		t.Fatalf("search content = %q, want %q", got, "partial answer")
	}
}
