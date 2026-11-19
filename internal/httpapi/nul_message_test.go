package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// A chat message of nothing but 0x00 bytes is empty as it would be stored,
// so it is refused like an empty message instead of starting a turn.
func TestChatRefusesANULOnlyMessage(t *testing.T) {
	t.Parallel()
	srv := newDefaultChatServer(t, &fakeEngine{}, newFakeChatStore())
	w := doAuthedJSONRequest(t, srv, http.MethodPost, "/chat", "alice@example.com", map[string]any{
		"message": "\x00\x00 \x00",
		"model":   "anthropic/claude-opus-4.8",
	})
	if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != "message is required" {
		t.Fatalf("status %d %q, want 400 message is required", w.Code, w.Body.String())
	}
}
