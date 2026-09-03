package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TestSuggestPrompt_ReturnsDraft drives the save-to-prompt-library endpoint: the
// synthesized draft comes back as JSON for the client's review dialog, and
// nothing is persisted server-side.
func TestSuggestPrompt_ReturnsDraft(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv := seedConv(t, s, user)
	s.agent = &fakeTurnEngine{libraryDraft: &agent.LibraryPromptDraft{
		Name:        "Failed-task report",
		Description: "Summarize scheduled tasks that failed today",
		Content:     "Summarize the scheduled tasks that failed in the last 24 hours, with the error for each.",
	}}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/suggest-prompt", nil)
	rec := httptest.NewRecorder()
	s.handleSuggestPrompt(rec, req, conv.ID, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Name != "Failed-task report" {
		t.Errorf("name = %q", body.Name)
	}
	if body.Description == "" {
		t.Error("expected a non-empty description")
	}
	if body.Content != "Summarize the scheduled tasks that failed in the last 24 hours, with the error for each." {
		t.Errorf("content = %q", body.Content)
	}
}

// TestSuggestPrompt_EmptyConversation returns 422 when there's nothing to distill.
func TestSuggestPrompt_EmptyConversation(t *testing.T) {
	s := serverFixture(t)
	const user = "carol@x.com"
	conv, err := s.store.CreateConversation(context.Background(), user, "empty", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	s.agent = &fakeTurnEngine{libraryDraft: &agent.LibraryPromptDraft{Content: "x"}}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/suggest-prompt", nil)
	rec := httptest.NewRecorder()
	s.handleSuggestPrompt(rec, req, conv.ID, user)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty conversation should be 422, got %d", rec.Code)
	}
}

// TestSuggestPrompt_NotOwned returns 404 for a conversation the caller doesn't own.
func TestSuggestPrompt_NotOwned(t *testing.T) {
	s := serverFixture(t)
	conv := seedConv(t, s, "owner@x.com")
	s.agent = &fakeTurnEngine{libraryDraft: &agent.LibraryPromptDraft{Content: "x"}}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/suggest-prompt", nil)
	rec := httptest.NewRecorder()
	s.handleSuggestPrompt(rec, req, conv.ID, "intruder@x.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign conversation should be 404, got %d", rec.Code)
	}
}

// TestSuggestPrompt_SynthesisFailure returns 502 when the model call fails, so
// the UI can show a retryable error instead of an empty dialog.
func TestSuggestPrompt_SynthesisFailure(t *testing.T) {
	s := serverFixture(t)
	const user = "dave@x.com"
	conv := seedConv(t, s, user)
	s.agent = &fakeTurnEngine{libraryDraftErr: errors.New("model unavailable")}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/suggest-prompt", nil)
	rec := httptest.NewRecorder()
	s.handleSuggestPrompt(rec, req, conv.ID, user)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("synthesis failure should be 502, got %d", rec.Code)
	}
}

// TestHistoryUpTo covers the per-message cut that backs the "Save as prompt"
// action on an assistant reply: the distillation must stop at the reply the
// user pointed at, and must degrade to the whole conversation rather than to
// nothing when the id doesn't name an entry it can see.
func TestHistoryUpTo(t *testing.T) {
	history := []agent.HistoryEntry{
		{ID: 1, Role: "user", Type: "text"},
		{ID: 2, Role: "assistant", Type: "text"},
		{ID: 3, Role: "user", Type: "text"},
		{ID: 4, Role: "assistant", Type: "text"},
	}
	cases := []struct {
		name string
		id   int64
		want int
	}{
		{"cuts inclusive at the named reply", 2, 2},
		{"zero means the whole conversation", 0, 4},
		{"negative means the whole conversation", -7, 4},
		{"an unknown id degrades to the whole conversation", 99, 4},
		{"the last entry keeps everything", 4, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(historyUpTo(history, tc.id)); got != tc.want {
				t.Errorf("historyUpTo(%d) kept %d entries, want %d", tc.id, got, tc.want)
			}
		})
	}
}

// TestSuggestPrompt_HonorsUpToMessageID drives the same cut through the
// handler: a later tangent must not reach the synthesizer when the user saved
// from an earlier reply.
func TestSuggestPrompt_HonorsUpToMessageID(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv := seedConv(t, s, user)
	engine := &fakeTurnEngine{libraryDraft: &agent.LibraryPromptDraft{Name: "n", Content: "c"}}
	s.agent = engine

	// The fixture chat is [user ask, assistant reply]. Cutting at the user
	// entry must keep the ask and drop everything after it.
	history, err := s.store.LoadHistory(context.Background(), conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) < 2 {
		t.Fatalf("fixture history = %d entries, want at least 2 to cut between", len(history))
	}

	body := strings.NewReader(`{"up_to_message_id":` + strconv.FormatInt(history[0].ID, 10) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/suggest-prompt", body)
	rec := httptest.NewRecorder()
	s.handleSuggestPrompt(rec, req, conv.ID, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(engine.libraryTranscript, "summarize scheduled tasks that failed today") {
		t.Errorf("transcript lost the ask being saved:\n%s", engine.libraryTranscript)
	}
	if strings.Contains(engine.libraryTranscript, "nightly-etl") {
		t.Errorf("transcript carried a turn PAST the cut:\n%s", engine.libraryTranscript)
	}
}

// TestSuggestPrompt_IgnoresMalformedBody proves the optional body stays
// advisory: junk in it means "the whole conversation", not a 400 in the user's
// face while they wait on a review dialog.
func TestSuggestPrompt_IgnoresMalformedBody(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv := seedConv(t, s, user)
	engine := &fakeTurnEngine{libraryDraft: &agent.LibraryPromptDraft{Name: "n", Content: "c"}}
	s.agent = engine

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/suggest-prompt", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	s.handleSuggestPrompt(rec, req, conv.ID, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(engine.libraryTranscript, "nightly-etl") {
		t.Errorf("a malformed body should distill the whole chat:\n%s", engine.libraryTranscript)
	}
}
