package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// mockServer wires a minimal Server with MockMode=true and a real Postgres
// store. No Manager is needed because /chat short-circuits before RunTurn.
// Skips when CHAT_TEST_DATABASE_URL is unset.
func mockServer(t *testing.T) *Server {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL is not set — skipping Postgres-backed test")
	}
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.TruncateAllForTest(context.Background()); err != nil {
		_ = st.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &Server{
		cfg: &config.Config{
			SharedToken:     "tok",
			PersonaDefault:  "generic",
			ConversationTTL: 14,
			UnpinnedCap:     50,
			MockMode:        true,
		},
		store:       st,
		sharedToken: "tok",
		inflight:    make(map[string]inflightEntry),
		isMember:    allowAllMembers,
	}
}

// TestMockTurn_SSEStream exercises the full mock SSE script: the frames a
// client (Playwright) would see should include conversation, turn.started,
// reasoning.{start,delta,end}, tool.call, tool.result, text.delta+,
// turn.completed.
func TestMockTurn_SSEStream(t *testing.T) {
	s := mockServer(t)

	body, _ := json.Marshal(map[string]any{
		"message": "hello mock",
		"persona": "generic",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "u@x.com")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	required := []string{
		"event: conversation",
		"event: turn.started",
		"event: reasoning.start",
		"event: reasoning.delta",
		"event: reasoning.end",
		"event: tool.call",
		`"name":"run_python"`,
		"event: tool.result",
		"event: text.delta",
		"event: turn.completed",
	}
	for _, m := range required {
		if !strings.Contains(got, m) {
			t.Errorf("missing %q in SSE body\n\n%s", m, got)
		}
	}

	// The assistant text is streamed word-by-word, so reconstruct it from
	// the text.delta events and assert the full reply appears.
	var reply strings.Builder
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &p); err == nil && p.Text != "" {
			// Only text.delta payloads use a bare {"text":"..."} shape.
			// Filter out tool_result/tool_call payloads by checking the
			// previous event line above.
			reply.WriteString(p.Text)
		}
	}
	// We'll also pick up reasoning deltas + tool names in the loop above,
	// but the exact substring we want is still present.
	if !strings.Contains(reply.String(), "Mock reply to: hello mock") {
		t.Errorf("assembled text missing reply: %q", reply.String())
	}
}

// TestMockTurn_PersistsHistory — after a mock turn runs, the conversation's
// history table should contain the canned events so pin/reload/resume works
// end-to-end in Playwright without involving OpenRouter.
func TestMockTurn_PersistsHistory(t *testing.T) {
	s := mockServer(t)

	body, _ := json.Marshal(map[string]any{"message": "first", "persona": "generic"})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "u@x.com")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	// Grab the created conversation id.
	ctx := context.Background()
	list, err := s.store.List(ctx, "u@x.com")
	if err != nil || len(list) == 0 {
		t.Fatalf("list: len=%d err=%v", len(list), err)
	}
	hist, err := s.store.LoadHistory(ctx, list[0].ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	// user + reasoning + tool_call + tool_result + assistant text = 5 entries.
	if len(hist) != 5 {
		t.Fatalf("history length: got %d want 5", len(hist))
	}
	wantTypes := []string{"text", "reasoning", "tool_call", "tool_result", "text"}
	for i, w := range wantTypes {
		if hist[i].Type != w {
			t.Errorf("entry %d: type=%q want %q", i, hist[i].Type, w)
		}
	}
}

// TestMockTurn_ReplyReflectsUserMessage — Playwright asserts on a specific
// substring, so guarantee the canned reply echoes the user input.
func TestMockTurn_ReplyReflectsUserMessage(t *testing.T) {
	if got := buildMockReply("foo bar"); got != "Mock reply to: foo bar" {
		t.Errorf("reply: got %q", got)
	}
	if got := buildMockReply(""); got != "Mock reply." {
		t.Errorf("empty reply: got %q", got)
	}
}

// TestMockTurn_AgentFieldUnused documents that the mock path does not touch
// the agent Manager. This lets test harnesses construct Server without
// dialing OpenRouter.
func TestMockTurn_AgentFieldUnused(t *testing.T) {
	s := mockServer(t)
	if s.agent != nil {
		t.Fatalf("expected nil Manager in mock test fixture (saw %T)", s.agent)
	}

	body, _ := json.Marshal(map[string]any{"message": "no-llm", "persona": "generic"})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "u@x.com")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}

	// Sanity: [agent.EventSink] is satisfied by the per-turn buffer.
	var _ agent.EventSink = (*turnBuffer)(nil)
}
