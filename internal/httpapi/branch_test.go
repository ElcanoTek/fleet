package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TestConversationBranch_EndToEnd drives the #454 flow through the mux: the
// owner GETs the conversation (which now exposes per-message ids), branches at a
// chosen message, and the new conversation is an independent fork carrying the
// copied prefix + lineage. Ownership and a bad branch point are enforced.
func TestConversationBranch_EndToEnd(t *testing.T) {
	s := serverFixture(t)
	st := s.concreteStore(t)
	ctx := context.Background()

	conv, err := st.CreateConversation(ctx, "alice@x.com", "Original", "victoria", "openai/gpt-5", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := st.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"q1"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"a1"}`)},
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"q2"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"a2"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	h := s.Routes()

	// GET the conversation; the history must now expose per-message ids so a
	// client can pick a branch point.
	get := do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "alice@x.com")
	if get.Code != http.StatusOK {
		t.Fatalf("GET conversation: %d (%s)", get.Code, get.Body.String())
	}
	var getResp struct {
		History []struct {
			ID   int64  `json:"id"`
			Role string `json:"role"`
		} `json:"history"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(getResp.History) != 4 {
		t.Fatalf("history len = %d, want 4", len(getResp.History))
	}
	for i, m := range getResp.History {
		if m.ID <= 0 {
			t.Fatalf("history[%d] has no id (branching needs message ids exposed): %+v", i, m)
		}
	}
	branchAt := getResp.History[1].ID // fork after the first two messages

	// Non-owner cannot branch alice's conversation (Get is user-scoped → 404).
	if w := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/branch",
		map[string]any{"branch_point_message_id": branchAt}, "mallory@x.com"); w.Code != http.StatusNotFound {
		t.Fatalf("non-owner branch: got %d, want 404", w.Code)
	}

	// Missing/zero branch point is a 400.
	if w := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/branch",
		map[string]any{"title": "x"}, "alice@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("missing branch point: got %d, want 400", w.Code)
	}

	// Owner branches → 201 + the new conversation, with lineage + inherited model.
	w := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/branch",
		map[string]any{"branch_point_message_id": branchAt}, "alice@x.com")
	if w.Code != http.StatusCreated {
		t.Fatalf("owner branch: got %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var branch struct {
		ID                   string `json:"id"`
		ParentConversationID string `json:"parent_conversation_id"`
		BranchPointMessageID int64  `json:"branch_point_message_id"`
		Model                string `json:"model"`
		Title                string `json:"title"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &branch); err != nil {
		t.Fatalf("decode branch response: %v", err)
	}
	if branch.ID == "" || branch.ID == conv.ID {
		t.Fatalf("branch must be a new conversation, got id=%q", branch.ID)
	}
	if branch.ParentConversationID != conv.ID || branch.BranchPointMessageID != branchAt {
		t.Errorf("branch lineage = (%q,%d), want (%q,%d)", branch.ParentConversationID, branch.BranchPointMessageID, conv.ID, branchAt)
	}
	if branch.Model != "openai/gpt-5" {
		t.Errorf("branch did not inherit model: %q", branch.Model)
	}
	if branch.Title == "" {
		t.Error("branch should have a default title")
	}

	// The branch carries exactly the copied prefix (2 messages); the parent is untouched.
	bget := do(t, h, http.MethodGet, "/conversations/"+branch.ID, nil, "alice@x.com")
	var bResp struct {
		History []json.RawMessage `json:"history"`
	}
	_ = json.Unmarshal(bget.Body.Bytes(), &bResp)
	if len(bResp.History) != 2 {
		t.Errorf("branch history len = %d, want 2", len(bResp.History))
	}
}
