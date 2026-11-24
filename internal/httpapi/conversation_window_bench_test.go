package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// seedLongConversation writes a conversation shaped like a long Workbench
// thread: turns of a prompt, reasoning, several tool calls with sizeable
// results (a query's rows, a script's output), now and then a dashboard panel,
// a reply and the turn summary. It returns the conversation id and how many
// history entries were written.
func seedLongConversation(tb testing.TB, st *store.Store, owner string, turns, resultBytes int) (string, int) {
	tb.Helper()
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, owner, "long thread", "generic", "", false)
	if err != nil {
		tb.Fatalf("create conversation: %v", err)
	}
	entry := func(role, typ string, v any) agent.HistoryEntry {
		b, err := json.Marshal(v)
		if err != nil {
			tb.Fatalf("marshal: %v", err)
		}
		return agent.HistoryEntry{Role: role, Type: typ, Content: b}
	}
	// Rows of a CSV-like result: compressible, as real tool output is.
	var row strings.Builder
	for i := 0; row.Len() < resultBytes; i++ {
		fmt.Fprintf(&row, "%d,campaign-%d,%d.%02d,%d\n", i, i%37, i*13%900, i%100, i*7919%100000)
	}
	result := row.String()
	total := 0
	for turn := range turns {
		batch := []agent.HistoryEntry{
			entry("user", "text", agent.TextContent{Text: fmt.Sprintf("refresh the pacing numbers, run %d", turn)}),
			entry("assistant", "reasoning", agent.TextContent{Text: strings.Repeat("Consider the pacing. ", 20)}),
		}
		for call := range 4 {
			id := fmt.Sprintf("call-%d-%d", turn, call)
			batch = append(batch,
				entry("assistant", "tool_call", agent.ToolCallContent{ID: id, Name: "run_python", Input: `{"code":"` + strings.Repeat("print(df.head())\\n", 20) + `"}`}),
				entry("tool", "tool_result", agent.ToolResultContent{ID: id, Name: "run_python", Text: result}),
			)
		}
		if turn%10 == 0 {
			id := fmt.Sprintf("ui-%d", turn)
			batch = append(batch,
				entry("assistant", "tool_call", agent.ToolCallContent{ID: id, Name: "show_ui", Input: `{"title":"Pacing","panel":"pacing","components":[{"type":"text","text":"run ` + fmt.Sprint(turn) + `"}]}`}),
				entry("tool", "tool_result", agent.ToolResultContent{ID: id, Name: "show_ui", Text: "UI_DISPLAYED card_id=" + id}),
			)
		}
		batch = append(batch,
			entry("assistant", "text", agent.TextContent{Text: strings.Repeat("Pacing is on track for the week. ", 40)}),
			entry("assistant", "turn_summary", agent.TurnSummaryContent{CostUSD: 0.01, PromptTokens: 1000, CompletionTokens: 200, DurationMs: 4000}),
		)
		if _, err := st.AppendHistory(ctx, conv.ID, batch); err != nil {
			tb.Fatalf("append history: %v", err)
		}
		total += len(batch)
	}
	return conv.ID, total
}

// BenchmarkConversationGet compares opening a long conversation whole (every
// caller before the tail window existed, and any caller that omits `limit`)
// with opening its latest window. Run with FLEET_TEST_DATABASE_URL set:
//
//	go test -tags fleet_host_executor ./internal/httpapi -run '^$' -bench ConversationGet -benchtime 20x
func BenchmarkConversationGet(b *testing.B) {
	const owner = "bench@x.com"
	st, err := store.Open(fleetdb.New(b).ChatDSN(), fleetdb.ChatPool())
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer st.Close()
	s := &Server{
		cfg:      &config.Config{PersonaDefault: "victoria"},
		store:    st,
		dir:      st.Identity(),
		inflight: make(map[string]inflightEntry),
		isMember: allowAllMembers,
	}
	h := s.Routes()
	id, entries := seedLongConversation(b, st, owner, 150, 20_000)
	for _, tc := range []struct{ name, query string }{
		{"full", ""},
		// The web client's first open (HISTORY_WINDOW_LIMIT).
		{"window", "?limit=80"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var size int
			for b.Loop() {
				path := "/conversations/" + id + tc.query
				req := httptest.NewRequestWithContext(gatewayContext(path, owner), http.MethodGet, path, http.NoBody)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					b.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				size = w.Body.Len()
			}
			b.ReportMetric(float64(size), "resp-bytes")
			b.ReportMetric(float64(entries), "stored-entries")
		})
	}
}
