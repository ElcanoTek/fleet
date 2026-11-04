package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// TestPatrolDraft_ReturnsDraft drives the Launchpad's "Draft it with AI"
// endpoint: the wizard's answers reach the synthesizer as given, and the
// draft comes back as JSON for the review step. Nothing is persisted.
func TestPatrolDraft_ReturnsDraft(t *testing.T) {
	fake := &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{
		Title:   "Morning deal sweep",
		Prompt:  "**Mission** …",
		Summary: "Every weekday morning I check your deals.",
	}}
	s := &Server{agent: fake}
	body := `{"goals":["Flag misconfigured deals","  ","Scan my inbox"],"notes":"focus on CTV",
		"connectors":[{"name":"gmail","description":"Read mail"}],"memories":["Never email clients"],
		"cadence":"Weekdays at 08:00","mode":"act","persona":"analyst"}`
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct{ Title, Prompt, Summary string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Title != "Morning deal sweep" || got.Prompt == "" || got.Summary == "" {
		t.Errorf("draft = %+v", got)
	}
	in := fake.patrolInput
	if len(in.Goals) != 2 || in.Goals[1] != "Scan my inbox" {
		t.Errorf("blank goals should be dropped, got %q", in.Goals)
	}
	if len(in.Connectors) != 1 || in.Connectors[0].Name != "gmail" || in.Connectors[0].Description != "Read mail" {
		t.Errorf("connectors = %+v", in.Connectors)
	}
	if in.Mode != "act" || in.Cadence != "Weekdays at 08:00" || in.Persona != "analyst" || in.Notes != "focus on CTV" {
		t.Errorf("input = %+v", in)
	}
	if len(in.Memories) != 1 {
		t.Errorf("memories = %q", in.Memories)
	}
}

// memoriesStore serves a fixed memory list to the drafter.
type memoriesStore struct {
	chatStore
	memories []store.Memory
	err      error
}

func (m *memoriesStore) ListMemories(context.Context, string) ([]store.Memory, error) {
	return m.memories, m.err
}

// TestPatrolDraft_UsesTheTurnsMemories pins that the drafter sees memories
// exactly as a patrol turn does (memoryContents): annotated with kind and
// validity window, without retired or proposed rows — not the raw text the
// wizard sent.
func TestPatrolDraft_UsesTheTurnsMemories(t *testing.T) {
	until := int64(1798761600) // 2027-01-01
	fake := &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{Title: "t", Prompt: "p", Summary: "s"}}
	s := &Server{agent: fake, store: &memoriesStore{memories: []store.Memory{
		{ID: "a", Content: "Q4 budget is $40k", Kind: "fact", ValidTo: &until},
		{ID: "b", Content: "Never email clients", Kind: "constraint", Pinned: true},
		{ID: "c", Content: "unaccepted", Source: "proposed"},
	}}}
	body := `{"goals":["Watch spend"],"memories":["Q4 budget is $40k"]}`
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := strings.Join(fake.patrolInput.Memories, "|")
	want := "Q4 budget is $40k (until 2027-01-01)|Never email clients (constraint)"
	if got != want {
		t.Errorf("memories = %q, want %q", got, want)
	}
}

// TestPatrolDraft_LongMemoryKeepsItsAnnotation pins that a long memory is
// shortened before its kind/validity annotation is added, so the annotation
// always reaches the drafter.
func TestPatrolDraft_LongMemoryKeepsItsAnnotation(t *testing.T) {
	until := int64(1798761600) // 2027-01-01
	fake := &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{Title: "t", Prompt: "p", Summary: "s"}}
	long := strings.Repeat("é", 3000)
	s := &Server{agent: fake, store: &memoriesStore{memories: []store.Memory{
		{ID: "a", Content: long, Kind: "fact", ValidTo: &until},
	}}}
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(`{"goals":["Watch spend"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := fake.patrolInput.Memories
	if len(got) != 1 || !strings.HasSuffix(got[0], "… (until 2027-01-01)") || len([]rune(got[0])) > patrolDraftMemoryRunes+40 {
		t.Errorf("memory = %q… (%d runes)", string([]rune(got[0])[:20]), len([]rune(got[0])))
	}
}

// TestPatrolDraft_UnreadableMemoriesRefuse pins that no draft is written when
// the memories cannot be read: a draft without them could contradict what
// every patrol turn is told.
func TestPatrolDraft_UnreadableMemoriesRefuse(t *testing.T) {
	fake := &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{Title: "t", Prompt: "p", Summary: "s"}}
	s := &Server{agent: fake, store: &memoriesStore{err: errors.New("db down")}}
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(`{"goals":["Watch spend"],"memories":["raw"]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if fake.patrolInput.Goals != nil {
		t.Error("the drafter ran without the memories")
	}
}

// TestPatrolDraft_Rejections pins the refusals: nothing to watch is a 400
// before any model call, a failed synthesis is a 502 the wizard reports, and
// the endpoint is POST-only.
func TestPatrolDraft_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		method string
		body   string
		fake   *fakeTurnEngine
		want   int
	}{
		{"nothing to watch", http.MethodPost, `{"goals":["  "],"notes":""}`, &fakeTurnEngine{}, http.StatusBadRequest},
		{"bad json", http.MethodPost, `{"goals":`, &fakeTurnEngine{}, http.StatusBadRequest},
		{"synthesis failed", http.MethodPost, `{"goals":["Scan my inbox"]}`, &fakeTurnEngine{patrolDraftErr: errors.New("boom")}, http.StatusBadGateway},
		{"no draft", http.MethodPost, `{"notes":"watch spend"}`, &fakeTurnEngine{}, http.StatusBadGateway},
		{"GET", http.MethodGet, ``, &fakeTurnEngine{}, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{agent: tc.fake}
			rec := httptest.NewRecorder()
			s.handlePatrolDraft(rec, httptest.NewRequest(tc.method, "/patrol-draft", strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// A refused request never reached the model.
	fake := &fakeTurnEngine{}
	s := &Server{agent: fake}
	s.handlePatrolDraft(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(`{}`)))
	if fake.patrolInput.Goals != nil || fake.patrolInput.Notes != "" {
		t.Error("an empty request should not reach the synthesizer")
	}
}

// TestPatrolDraft_BodyCap refuses an oversized request rather than reading it.
func TestPatrolDraft_BodyCap(t *testing.T) {
	s := &Server{agent: &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{Title: "t", Prompt: "p"}}}
	big := `{"goals":["` + strings.Repeat("x", patrolDraftMaxBody) + `"]}`
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(big)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestPatrols_ListsTheCallersPatrols backs the Patrols hub in the rail: the
// caller's live patrols, each with its chat's title, and nobody else's.
func TestPatrols_ListsTheCallersPatrols(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	const me = "hub@x.com"
	conv := seedConv(t, s, me)
	if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: conv.ID, UserEmail: me, Title: "Deal sweep", Prompt: "check", CronExpr: "0 8 * * 1-5", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}
	theirs := seedConv(t, s, "someone@x.com")
	if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: theirs.ID, UserEmail: "someone@x.com", Title: "Not mine", Prompt: "check", CronExpr: "0 8 * * *", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/patrols", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, me))
	rec := httptest.NewRecorder()
	s.handlePatrols(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Patrols []struct {
			ConversationID    string `json:"conversation_id"`
			Title             string `json:"title"`
			ConversationTitle string `json:"conversation_title"`
			Status            string `json:"status"`
		} `json:"patrols"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Patrols) != 1 || body.Patrols[0].Title != "Deal sweep" || body.Patrols[0].ConversationID != conv.ID ||
		body.Patrols[0].ConversationTitle != conv.Title || body.Patrols[0].Status != "active" {
		t.Fatalf("patrols = %+v", body.Patrols)
	}

	// Patrols the caller made in teammates' chats: listed while the caller can
	// still open that chat (multiplayer participant), dropped once they cannot.
	seedUser(t, s.concreteStore(t), me)
	shared := seedConv(t, s, "teammate@x.com")
	if _, err := s.store.SetConversationMultiplayer(ctx, "teammate@x.com", shared.ID, true, []string{me}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: shared.ID, UserEmail: me, Title: "Shared sweep", Prompt: "check", CronExpr: "0 9 * * *", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("CreateChatSchedule (shared): %v", err)
	}
	// Made while multiplayer, then the owner turned multiplayer off.
	private := seedConv(t, s, "teammate@x.com")
	if _, err := s.store.SetConversationMultiplayer(ctx, "teammate@x.com", private.ID, true, []string{me}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: private.ID, UserEmail: me, Title: "No longer mine to see", Prompt: "check", CronExpr: "0 9 * * *", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("CreateChatSchedule (private): %v", err)
	}
	if _, err := s.store.SetConversationMultiplayer(ctx, "teammate@x.com", private.ID, false, nil, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer off: %v", err)
	}
	rec = httptest.NewRecorder()
	s.handlePatrols(rec, req)
	body.Patrols = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	titles := map[string]bool{}
	for _, p := range body.Patrols {
		titles[p.Title] = true
	}
	if !titles["Deal sweep"] || !titles["Shared sweep"] || titles["No longer mine to see"] || len(body.Patrols) != 2 {
		t.Fatalf("patrols = %+v, want Deal sweep + Shared sweep only", body.Patrols)
	}
	if strings.Contains(rec.Body.String(), `"prompt"`) {
		t.Error("the index must not carry patrol prompts")
	}

	post := httptest.NewRecorder()
	s.handlePatrols(post, httptest.NewRequest(http.MethodPost, "/patrols", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", post.Code)
	}
}

// TestPatrolDraft_OneAtATime refuses a second draft while the caller's first
// is still being written: each is a model call of up to 45 seconds.
func TestPatrolDraft_OneAtATime(t *testing.T) {
	const user = "busy@x.com"
	patrolDraftInFlight.Store(user, struct{}{})
	defer patrolDraftInFlight.Delete(user)
	fake := &fakeTurnEngine{patrolDraft: &agent.PatrolDraft{Title: "t", Prompt: "p"}}
	s := &Server{agent: fake}
	req := httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(`{"goals":["Scan my inbox"]}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	rec := httptest.NewRecorder()
	s.handlePatrolDraft(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if fake.patrolInput.Goals != nil {
		t.Error("a refused draft must not reach the model")
	}
	// Once the first finishes, the next is accepted.
	patrolDraftInFlight.Delete(user)
	rec = httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/patrol-draft", strings.NewReader(`{"goals":["Scan my inbox"]}`))
	s.handlePatrolDraft(rec, req2.WithContext(context.WithValue(req2.Context(), ctxKeyUser, user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("after the first finished, status = %d, want 200", rec.Code)
	}
	if _, still := patrolDraftInFlight.Load(user); still {
		t.Error("the in-flight mark must be released when the draft returns")
	}
}

// TestPatrols_RevokedRowsDoNotCrowdOut: patrols in chats the caller can no
// longer open are dropped after the query, so the index must page past them —
// a full page of revoked rows sorted first must not hide a patrol the caller
// can still open (or make the rail think there are none and open the wizard).
func TestPatrols_RevokedRowsDoNotCrowdOut(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	const me, owner = "crowd@x.com", "owner@x.com"
	seedUser(t, s.concreteStore(t), me)
	for i := 0; i < patrolsIndexMax; i++ {
		c := seedConv(t, s, owner)
		if _, err := s.store.SetConversationMultiplayer(ctx, owner, c.ID, true, []string{me}, nil); err != nil {
			t.Fatalf("SetConversationMultiplayer: %v", err)
		}
		// Hourly: sorts ahead of the weekly patrol below.
		if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
			ConversationID: c.ID, UserEmail: me, Title: "revoked", Prompt: "check", CronExpr: "0 * * * *", Timezone: "UTC",
		}); err != nil {
			t.Fatalf("CreateChatSchedule: %v", err)
		}
		if _, err := s.store.SetConversationMultiplayer(ctx, owner, c.ID, false, nil, nil); err != nil {
			t.Fatalf("SetConversationMultiplayer off: %v", err)
		}
	}
	mine := seedConv(t, s, me)
	if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: mine.ID, UserEmail: me, Title: "Still mine", Prompt: "check", CronExpr: "0 9 * * 1", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/patrols", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, me))
	rec := httptest.NewRecorder()
	s.handlePatrols(rec, req)
	var body struct {
		Patrols []struct {
			Title string `json:"title"`
		} `json:"patrols"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(body.Patrols) != 1 || body.Patrols[0].Title != "Still mine" {
		t.Fatalf("patrols = %+v, want only Still mine", body.Patrols)
	}
}

// failingMultiplayerStore makes the per-chat access check error out, to show
// an error is not mistaken for "access revoked".
type failingMultiplayerStore struct{ chatStore }

func (failingMultiplayerStore) GetMultiplayerConversation(context.Context, string, string) (*store.Conversation, error) {
	return nil, errors.New("db down")
}

// TestPatrols_AccessCheckErrorIs5xx: if re-checking access to a teammate's
// chat fails, the index answers 5xx (the rail shows its load-failure state)
// rather than a successful list that silently drops patrols — or a false
// empty one that opens the first-time wizard.
func TestPatrols_AccessCheckErrorIs5xx(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	const me, owner = "err@x.com", "errowner@x.com"
	seedUser(t, s.concreteStore(t), me)
	c := seedConv(t, s, owner)
	if _, err := s.store.SetConversationMultiplayer(ctx, owner, c.ID, true, []string{me}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: c.ID, UserEmail: me, Title: "Shared", Prompt: "check", CronExpr: "0 9 * * *", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("CreateChatSchedule: %v", err)
	}
	s.store = failingMultiplayerStore{s.store}
	req := httptest.NewRequest(http.MethodGet, "/patrols", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, me))
	rec := httptest.NewRecorder()
	s.handlePatrols(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
	}
}

// countingMultiplayerStore counts multiplayer access checks.
type countingMultiplayerStore struct {
	chatStore
	calls *int
}

func (c countingMultiplayerStore) GetMultiplayerConversation(ctx context.Context, user, convID string) (*store.Conversation, error) {
	*c.calls++
	return c.chatStore.GetMultiplayerConversation(ctx, user, convID)
}

// TestPatrols_AccessChecksOncePerTeammateChat pins the index's cost: patrols
// in the caller's own chats need no access check, and patrols sharing a
// teammate's chat share one.
func TestPatrols_AccessChecksOncePerTeammateChat(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	const me, owner = "cost@x.com", "costowner@x.com"
	seedUser(t, s.concreteStore(t), me)
	mine := seedConv(t, s, me)
	shared := seedConv(t, s, owner)
	if _, err := s.store.SetConversationMultiplayer(ctx, owner, shared.ID, true, []string{me}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	for i, convID := range []string{mine.ID, mine.ID, shared.ID, shared.ID, shared.ID} {
		if _, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
			ConversationID: convID, UserEmail: me, Title: fmt.Sprintf("P%d", i), Prompt: "check", CronExpr: "0 9 * * *", Timezone: "UTC",
			AllowMultiple: true,
		}); err != nil {
			t.Fatalf("CreateChatSchedule: %v", err)
		}
	}
	calls := 0
	s.store = countingMultiplayerStore{chatStore: s.store, calls: &calls}
	req := httptest.NewRequest(http.MethodGet, "/patrols", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, me))
	rec := httptest.NewRecorder()
	s.handlePatrols(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Patrols []struct{} `json:"patrols"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Patrols) != 5 {
		t.Errorf("patrols = %d, want 5", len(body.Patrols))
	}
	if calls != 1 {
		t.Errorf("multiplayer access checks = %d, want 1 (one teammate chat; own chats need none)", calls)
	}
}
