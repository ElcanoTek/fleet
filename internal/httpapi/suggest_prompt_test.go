package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
