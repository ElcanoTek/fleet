package httpapi

// #2044: the summarize endpoint checks for an in-flight turn once, then spends
// 30-60 s on the model call. A turn that starts and commits in that window
// used to be silently dropped from the model's context — the summary was
// inserted with a fresh id after it, and replayHistory discards everything
// before the latest summary. The real agent loop runs the turn here against
// the fake LLM while a blocking summarizer holds the summary call open.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// blockingSummarizeEngine is the resume harness's real engine with a
// Summarize that reports it started and then waits to be released.
type blockingSummarizeEngine struct {
	*resumeEngine
	started chan struct{}
	release chan struct{}
}

func (e *blockingSummarizeEngine) Summarize(ctx context.Context, _ SummarizeInput) (*SummarizeResult, error) {
	select {
	case e.started <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &SummarizeResult{Text: "summary of the chat", Model: resumeTestModel}, nil
}

func TestSummarize_TurnCommittedDuringSummaryIsNotDropped(t *testing.T) {
	h := newResumeHarness(t, agentcore.AgentPolicy{}, time.Millisecond)
	eng := &blockingSummarizeEngine{
		resumeEngine: h.s.agent.(*resumeEngine),
		started:      make(chan struct{}, 1),
		release:      make(chan struct{}),
	}
	h.s.agent = eng
	conv := h.conversation()
	ctx := context.Background()
	if _, err := h.s.store.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{
		histEntry("user", "text", agent.TextContent{Text: "first question"}),
		histEntry("assistant", "text", agent.TextContent{Text: "first answer"}),
	}); err != nil {
		t.Fatal(err)
	}
	routes := h.s.Routes()

	summarize := func() <-chan string {
		out := make(chan string, 1)
		go func() {
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/conversations/"+conv.ID+"/summarize", strings.NewReader(`{}`))
			req = req.WithContext(asUser(req.Context(), resumeTestUser))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			routes.ServeHTTP(w, req)
			out <- w.Body.String()
		}()
		return out
	}

	first := summarize()
	select {
	case <-eng.started:
	case <-time.After(waitForBudget):
		t.Fatal("the summary call never started")
	}

	// A turn starts and commits while the summary is being written. The
	// in-flight check already passed, so nothing stops it.
	if w := do(t, routes, http.MethodPost, "/chat", map[string]any{
		"conversation_id": conv.ID,
		"message":         "second question",
	}, resumeTestUser); w.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", w.Code, w.Body.String())
	}
	h.idle(conv.ID)
	close(eng.release)

	var body string
	select {
	case body = <-first:
	case <-time.After(waitForBudget):
		t.Fatal("summarize never finished")
	}
	if !strings.Contains(body, "event: summary.error") || !strings.Contains(body, summaryStaleMessage) {
		t.Fatalf("summarize over a moved-on history must report the conflict, got:\n%s", body)
	}
	if strings.Contains(body, "summary.completed") {
		t.Fatalf("a refused summary must not report completion:\n%s", body)
	}

	hist, err := h.s.store.LoadHistory(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range hist {
		if e.Type == "summary" {
			t.Fatalf("the stale summary was persisted: %+v", hist)
		}
	}
	if !historyHasUserText(hist, "second question") {
		t.Fatalf("the concurrent turn is missing from history: %+v", hist)
	}

	// Summarizing again covers the new turn and is saved.
	select {
	case body = <-summarize():
	case <-time.After(waitForBudget):
		t.Fatal("second summarize never finished")
	}
	if !strings.Contains(body, "event: summary.completed") {
		t.Fatalf("re-summarizing the current history should succeed, got:\n%s", body)
	}
	hist, err = h.s.store.LoadHistory(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := hist[len(hist)-1]; last.Type != "summary" {
		t.Fatalf("expected the new summary to be the latest message, got %s", last.Type)
	}
}

func historyHasUserText(hist []agent.HistoryEntry, text string) bool {
	for _, e := range hist {
		if e.Role != "user" || e.Type != "text" {
			continue
		}
		var c agent.TextContent
		if json.Unmarshal(e.Content, &c) == nil && strings.Contains(c.Text, text) {
			return true
		}
	}
	return false
}

// A retry's truncate while the summary is being written removes messages the
// summary text still describes, without adding a newer one. The summary must
// not be saved over the clipped history.
func TestSummarize_TruncateDuringSummaryIsReported(t *testing.T) {
	h := newResumeHarness(t, agentcore.AgentPolicy{}, time.Millisecond)
	eng := &blockingSummarizeEngine{
		resumeEngine: h.s.agent.(*resumeEngine),
		started:      make(chan struct{}, 1),
		release:      make(chan struct{}),
	}
	h.s.agent = eng
	conv := h.conversation()
	ctx := context.Background()
	if _, err := h.s.store.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{
		histEntry("user", "text", agent.TextContent{Text: "first question"}),
		histEntry("assistant", "text", agent.TextContent{Text: "first answer"}),
		histEntry("user", "text", agent.TextContent{Text: "second question"}),
		histEntry("assistant", "text", agent.TextContent{Text: "reply the user retries"}),
	}); err != nil {
		t.Fatal(err)
	}
	routes := h.s.Routes()

	done := make(chan string, 1)
	go func() {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/conversations/"+conv.ID+"/summarize", strings.NewReader(`{}`))
		req = req.WithContext(asUser(req.Context(), resumeTestUser))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		done <- w.Body.String()
	}()
	select {
	case <-eng.started:
	case <-time.After(waitForBudget):
		t.Fatal("the summary call never started")
	}

	// Retry: drop the assistant tail after the latest user message.
	if w := do(t, routes, http.MethodPost, "/conversations/"+conv.ID+"/truncate", nil, resumeTestUser); w.Code != http.StatusNoContent {
		t.Fatalf("truncate: %d %s", w.Code, w.Body.String())
	}
	close(eng.release)

	var body string
	select {
	case body = <-done:
	case <-time.After(waitForBudget):
		t.Fatal("summarize never finished")
	}
	if !strings.Contains(body, "event: summary.error") || !strings.Contains(body, summaryStaleMessage) {
		t.Fatalf("summarize over a truncated history must report the conflict, got:\n%s", body)
	}
	hist, err := h.s.store.LoadHistory(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range hist {
		if e.Type == "summary" {
			t.Fatalf("a summary was saved over the truncated history: %+v", hist)
		}
	}
	if len(hist) != 3 {
		t.Fatalf("history after truncate = %d entries, want 3", len(hist))
	}
}
