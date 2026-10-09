package chattui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/contracttest"
)

// The chat stream contract, consumer side for the Go clients (`fleet chat`
// and, through it, `fleet acp`): every recording in
// testdata/contracts/chat-stream — what the real producer emits — is served
// verbatim as the chat server would, and the client must hand every frame to
// its caller unchanged and end the call with the outcome the terminal event
// means. See internal/agent TestChatStreamContract for the producer side.
func TestClientStreamReplaysRecordedContract(t *testing.T) {
	for _, name := range contracttest.ChatStreamRecordings(t) {
		t.Run(name, func(t *testing.T) {
			raw, frames := contracttest.ReadChatStream(t, name)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Fleet-Conversation-Id", "conv-contract")
				_, _ = w.Write(raw)
			}))
			defer srv.Close()

			var got []contracttest.Frame
			c := NewClient(Config{ServerURL: srv.URL, Email: "u@example.com"})
			convID, err := c.Stream(context.Background(), "replay", "", func(ev Event) {
				if ev.Name == "conversation" || ev.Name == "turn.identified" {
					return // synthesized from the response headers, not frames
				}
				got = append(got, contracttest.Frame{Event: ev.Name, Data: ev.Data})
			})

			if convID != "conv-contract" {
				t.Errorf("conversation id = %q, want the header's", convID)
			}
			if len(got) != len(frames) {
				t.Fatalf("delivered %d events, the recording has %d", len(got), len(frames))
			}
			for i, f := range frames {
				if got[i].Event != f.Event || !reflect.DeepEqual(got[i].Data, f.Data) {
					t.Errorf("event %d: got %s %v, recorded %s %v", i+1, got[i].Event, got[i].Data, f.Event, f.Data)
				}
			}

			terminal := contracttest.Expect(frames).Terminal
			message, _ := terminal.Data["message"].(string)
			switch terminal.Event {
			case "turn.completed":
				if err != nil {
					t.Errorf("a completed turn returned %v", err)
				}
			case "turn.cancelled":
				if !errors.Is(err, context.Canceled) {
					t.Errorf("a cancelled turn returned %v, want context.Canceled", err)
				}
			case "turn.model_required", "turn.error":
				if err == nil || !strings.Contains(err.Error(), message) {
					t.Errorf("%s returned %v, want an error carrying %q", terminal.Event, err, message)
				}
			default:
				t.Fatalf("%s ends with %q; teach this test what the client must return for it", name, terminal.Event)
			}
		})
	}
}
