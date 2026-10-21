package httpapi

import (
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// TestChatTurnCarriesSignedInSender pins the HTTP half of ADR-0087: a turn a
// signed-in user sent hands that user to the engine as TurnInput.MCPUser — the
// value the turn's MCP scope substitutes for ${FLEET_USER_*} — with the first
// name they saved in Settings.
func TestChatTurnCarriesSignedInSender(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	st.givenNames = map[string]string{"u@x.com": "Ursula"}
	srv := newDefaultChatServer(t, engine, st)

	w := postChatRequest(t, srv, map[string]any{
		"message": "book the deals",
		"persona": "generic",
		"model":   "anthropic/claude-opus-4.8",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if want := (agentcore.MCPUserIdentity{Email: "u@x.com", Name: "Ursula"}); engine.lastMCPUser != want {
		t.Fatalf("MCPUser = %+v, want %+v (the authenticated sender and their saved name)", engine.lastMCPUser, want)
	}
}

// TestChatTurnWithoutSavedNameHasNoName: a sender who saved no first name gets
// none. Nothing is derived from the email, so the connector's
// ${FLEET_USER_NAME} key is dropped and its greeting says "Hi team,".
func TestChatTurnWithoutSavedNameHasNoName(t *testing.T) {
	engine := &fakeEngine{}
	srv := newDefaultChatServer(t, engine, newFakeChatStore())

	w := postChatRequest(t, srv, map[string]any{
		"message": "book the deals",
		"persona": "generic",
		"model":   "anthropic/claude-opus-4.8",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if want := (agentcore.MCPUserIdentity{Email: "u@x.com"}); engine.lastMCPUser != want {
		t.Fatalf("MCPUser = %+v, want %+v (email only, no guessed name)", engine.lastMCPUser, want)
	}
}

// TestWebhookTurnCarriesNoSignedInUser: a webhook-started turn runs as the
// trigger owner for scoping (UserEmail), but nobody signed in sent it, so it
// carries no MCP user and its ${FLEET_USER_*} keys are dropped.
func TestWebhookTurnCarriesNoSignedInUser(t *testing.T) {
	engine := &fakeEngine{}
	srv := newWebhookServer(t, engine, newFakeChatStore())

	body := []byte(`{"title":"Fix the widget"}`)
	w := postWebhook(t, srv, "gh", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body),
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", w.Code, w.Body.String())
	}
	eventually(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.turns == 1
	}, "webhook turn never ran")
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.lastUserEmail != webhookOwner {
		t.Fatalf("UserEmail = %q, want the trigger owner %q", engine.lastUserEmail, webhookOwner)
	}
	if !engine.lastMCPUser.IsZero() {
		t.Fatalf("MCPUser = %+v, want none for a webhook-started turn", engine.lastMCPUser)
	}
}
