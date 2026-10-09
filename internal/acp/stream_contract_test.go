package acp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/chattui"
	"github.com/ElcanoTek/fleet/internal/contracttest"
)

// The chat stream contract, consumer side for `fleet acp`: every recording in
// testdata/contracts/chat-stream — what the real producer emits — goes through
// the same path a prompt takes in production (the chattui client reading the
// stream, the translator turning it into ACP session updates), and the editor
// must end up seeing the recorded answer, every tool call with the outcome
// its result reported, and the turn ended by the recorded terminal event. See
// internal/agent TestChatStreamContract for the producer side.
func TestTranslatorReplaysRecordedContract(t *testing.T) {
	for _, name := range contracttest.ChatStreamRecordings(t) {
		t.Run(name, func(t *testing.T) {
			raw, frames := contracttest.ReadChatStream(t, name)
			want := contracttest.Expect(frames)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Fleet-Conversation-Id", "conv-contract")
				_, _ = w.Write(raw)
			}))
			defer srv.Close()

			var mu sync.Mutex
			var updates []acpsdk.SessionUpdate
			tr := newTranslator("session-contract", "", func(u acpsdk.SessionUpdate) {
				mu.Lock()
				defer mu.Unlock()
				updates = append(updates, u)
			})
			client := chattui.NewClient(chattui.Config{ServerURL: srv.URL, Email: "u@example.com"})
			_, _ = client.Stream(context.Background(), "replay", "", tr.handle)

			mu.Lock()
			defer mu.Unlock()
			var text strings.Builder
			started := map[string]string{}
			final := map[string]acpsdk.ToolCallStatus{}
			for _, u := range updates {
				switch {
				case u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil:
					text.WriteString(u.AgentMessageChunk.Content.Text.Text)
				case u.ToolCall != nil:
					started[string(u.ToolCall.ToolCallId)] = u.ToolCall.Title
				case u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil:
					final[string(u.ToolCallUpdate.ToolCallId)] = *u.ToolCallUpdate.Status
				}
			}

			// A turn that ends without text reports its outcome through the
			// prompt response, not as agent text, so only streamed text counts.
			if streamed := strings.TrimSpace(want.Text); streamed != "" && text.String() != want.Text {
				t.Errorf("editor shows %q, the producer streamed %q", text.String(), want.Text)
			}
			for _, tool := range want.Tools {
				if started[tool.ID] != tool.Name {
					t.Errorf("tool call %s: started as %q, want %q", tool.ID, started[tool.ID], tool.Name)
				}
				if !tool.Result {
					continue
				}
				wantStatus := acpsdk.ToolCallStatusCompleted
				if tool.IsErr {
					wantStatus = acpsdk.ToolCallStatusFailed
				}
				if final[tool.ID] != wantStatus {
					t.Errorf("tool call %s (%s): ended %q, want %q", tool.ID, tool.Name, final[tool.ID], wantStatus)
				}
			}
			if !tr.ended() || tr.endedBy != want.Terminal.Event {
				t.Errorf("translator ended=%v by %q, want ended by %q", tr.ended(), tr.endedBy, want.Terminal.Event)
			}
			if want.Terminal.Event == "turn.completed" && tr.usage == nil {
				t.Error("a completed turn reported no usage to the editor")
			}
		})
	}
}
