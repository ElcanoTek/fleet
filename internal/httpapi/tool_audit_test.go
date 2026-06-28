package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// TestDeriveToolCallEntries is a pure (DB-free) check that the audit rows are
// correctly paired from history AND that secret-bearing tool ARGS are redacted
// before they would ever reach the ledger — the core host-side-credentials
// guarantee for this feature.
func TestDeriveToolCallEntries(t *testing.T) {
	mk := func(role, typ string, content any) agent.HistoryEntry {
		raw, _ := json.Marshal(content)
		return agent.HistoryEntry{Role: role, Type: typ, Content: raw}
	}
	history := []agent.HistoryEntry{
		mk("user", "text", agent.TextContent{Text: "hi"}),
		// A normal call + its result.
		mk("assistant", "tool_call", agent.ToolCallContent{ID: "c1", Name: "bash", Input: `{"command":"ls"}`}),
		mk("tool", "tool_result", agent.ToolResultContent{ID: "c1", Name: "bash", Text: "file1", IsErr: false}),
		// A call whose ARGS carry a secret — must be redacted in the summary.
		mk("assistant", "tool_call", agent.ToolCallContent{ID: "c2", Name: "http_get", Input: `{"api_key":"sk-ant-abcdef0123456789abcdef0123"}`}),
		mk("tool", "tool_result", agent.ToolResultContent{ID: "c2", Name: "http_get", Text: "ok", IsErr: false}),
		// A call that errored.
		mk("assistant", "tool_call", agent.ToolCallContent{ID: "c3", Name: "run_python", Input: `{"code":"1/0"}`}),
		mk("tool", "tool_result", agent.ToolResultContent{ID: "c3", Name: "run_python", Text: "ZeroDivisionError", IsErr: true}),
		// A call with NO matching result — interrupted/cancelled.
		mk("assistant", "tool_call", agent.ToolCallContent{ID: "c4", Name: "send_email", Input: `{"to":"x@y.com"}`}),
	}

	got := deriveToolCallEntries(history, "conv-1", "turn-1", "u@x.com", 1000)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}
	for i, e := range got {
		if e.ConversationID != "conv-1" || e.TurnID != "turn-1" || e.UserEmail != "u@x.com" || e.StartedAt != 1000 {
			t.Errorf("entry %d common fields wrong: %+v", i, e)
		}
	}

	// c1: paired, no error.
	if got[0].ToolName != "bash" || got[0].IsError || got[0].ResultSummary != "file1" {
		t.Errorf("c1 wrong: %+v", got[0])
	}

	// c2: secret in args must be scrubbed, NOT stored verbatim.
	if strings.Contains(got[1].ArgsSummary, "sk-ant-") {
		t.Errorf("c2 args leaked a secret: %q", got[1].ArgsSummary)
	}
	if !strings.Contains(got[1].ArgsSummary, "[REDACTED]") {
		t.Errorf("c2 args not redacted: %q", got[1].ArgsSummary)
	}

	// c3: errored.
	if !got[2].IsError || got[2].ResultSummary != "ZeroDivisionError" {
		t.Errorf("c3 wrong: %+v", got[2])
	}

	// c4: no result → flagged error, empty result summary.
	if got[3].ToolName != "send_email" || !got[3].IsError || got[3].ResultSummary != "" {
		t.Errorf("c4 (unpaired) wrong: %+v", got[3])
	}
}

// TestConversationAuditEndpoint exercises GET /conversations/{id}/audit against a
// real store: the owner gets 200 + paginated rows, a different user gets 404
// (membership scope), and the tool filter narrows results.
func TestConversationAuditEndpoint(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	st := s.concreteStore(t)

	seedUser(t, st, "owner@x.com")
	seedUser(t, st, "intruder@x.com")

	conv, err := st.CreateConversation(context.Background(), "owner@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	now := time.Now().Unix()
	if err := st.RecordToolCalls(context.Background(), []store.ToolCallEntry{
		{ConversationID: conv.ID, TurnID: "tr-1", UserEmail: "owner@x.com", ToolName: "bash", ArgsSummary: `{"command":"ls"}`, ResultSummary: "ok", StartedAt: now},
		{ConversationID: conv.ID, TurnID: "tr-1", UserEmail: "owner@x.com", ToolName: "run_python", ArgsSummary: `{"code":"x"}`, IsError: true, StartedAt: now + 1},
	}); err != nil {
		t.Fatalf("RecordToolCalls: %v", err)
	}

	// Owner: 200 + both rows, newest first.
	w := do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/audit", nil, "owner@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("owner audit: %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ToolCalls []map[string]any `json:"tool_calls"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool_calls, got %d (%s)", len(resp.ToolCalls), w.Body.String())
	}
	if resp.ToolCalls[0]["tool_name"] != "run_python" {
		t.Errorf("expected newest-first, got %v", resp.ToolCalls[0]["tool_name"])
	}

	// Intruder: 404 (conversation not visible — membership scope).
	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/audit", nil, "intruder@x.com")
	if w.Code != http.StatusNotFound {
		t.Fatalf("intruder audit: expected 404, got %d body=%s", w.Code, w.Body.String())
	}

	// Tool filter.
	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID+"/audit?tool=bash", nil, "owner@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("filtered audit: %d", w.Code)
	}
	resp.ToolCalls = nil
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0]["tool_name"] != "bash" {
		t.Fatalf("tool filter failed: %s", w.Body.String())
	}
}
