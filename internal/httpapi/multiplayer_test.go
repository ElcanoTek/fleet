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
	"github.com/ElcanoTek/fleet/internal/store"
)

func doAuthedJSONRequest(t *testing.T, srv *Server, method, path, userEmail string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf.Write(raw)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, &buf)
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", userEmail)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, req)
	return w
}

func TestMultiplayer_DefaultOnAndPerChatToggle(t *testing.T) {
	t.Run("default-on instance creates multiplayer chats by default (except lockdown)", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		srv.cfg.SetTeamChatsSharedByDefault(true)
		srv.cfg.SandboxImage = "localhost/fleet-sandbox:latest"
		srv.cfg.LockdownAllowedModels = []string{"anthropic/claude-opus-4.8"}

		// 1. POST /chat creates a multiplayer conversation by default.
		w := doAuthedJSONRequest(t, srv, http.MethodPost, "/chat", "alice@reklaim.com", map[string]any{
			"message": "hello team",
			"model":   "anthropic/claude-opus-4.8",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("POST /chat status = %d: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		conv1 := st.convs["conv-1"]
		st.mu.Unlock()
		if conv1 == nil || !conv1.Multiplayer || !conv1.TeamVisible {
			t.Fatalf("expected conv-1 to have Multiplayer=true and TeamVisible=true, got %+v", conv1)
		}

		// 2. Lockdown chat stays private even when default-on is enabled.
		wLock := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations", "alice@reklaim.com", map[string]any{
			"title":    "lockdown secret",
			"lockdown": true,
			"model":    "anthropic/claude-opus-4.8",
		})
		if wLock.Code != http.StatusOK {
			t.Fatalf("POST /conversations lockdown status = %d: %s", wLock.Code, wLock.Body.String())
		}
		var lockConv store.Conversation
		if err := json.Unmarshal(wLock.Body.Bytes(), &lockConv); err != nil {
			t.Fatalf("decode lockdown conv: %v", err)
		}
		if lockConv.Multiplayer || lockConv.TeamVisible {
			t.Fatalf("expected lockdown conversation to stay private, got Multiplayer=%v TeamVisible=%v", lockConv.Multiplayer, lockConv.TeamVisible)
		}
	})

	t.Run("multiplayer turn uses sender identity and supports adding/removing people", func(t *testing.T) {
		engine := &fakeEngine{}
		st := newFakeChatStore()
		srv := newDefaultChatServer(t, engine, st)
		// Default-off instance: new chats start private.
		wCreate := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations", "alice@reklaim.com", map[string]any{
			"title": "Q4 Deal Optimization",
			"model": "anthropic/claude-opus-4.8",
		})
		if wCreate.Code != http.StatusOK {
			t.Fatalf("create conv status = %d: %s", wCreate.Code, wCreate.Body.String())
		}
		var conv store.Conversation
		if err := json.Unmarshal(wCreate.Body.Bytes(), &conv); err != nil {
			t.Fatalf("decode conv: %v", err)
		}
		if conv.Multiplayer {
			t.Fatalf("expected private conversation when default-on is false")
		}

		// Bob cannot access Alice's private conversation before multiplayer is enabled.
		wBobPrivate := doAuthedJSONRequest(t, srv, http.MethodGet, "/conversations/"+conv.ID, "bob@reklaim.com", nil)
		if wBobPrivate.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for Bob on private conv, got %d", wBobPrivate.Code)
		}

		// Alice enables Multiplayer mode on the conversation.
		wEnable := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations/"+conv.ID+"/multiplayer", "alice@reklaim.com", map[string]any{
			"multiplayer": true,
		})
		if wEnable.Code != http.StatusOK {
			t.Fatalf("enable multiplayer status = %d: %s", wEnable.Code, wEnable.Body.String())
		}

		// Bob sees the conversation in GET /conversations?scope=shared.
		wShared := doAuthedJSONRequest(t, srv, http.MethodGet, "/conversations?scope=shared", "bob@reklaim.com", nil)
		if wShared.Code != http.StatusOK {
			t.Fatalf("GET /conversations?scope=shared status = %d: %s", wShared.Code, wShared.Body.String())
		}
		var sharedResp struct {
			Conversations []store.Conversation `json:"conversations"`
		}
		if err := json.Unmarshal(wShared.Body.Bytes(), &sharedResp); err != nil {
			t.Fatalf("decode shared conversations: %v", err)
		}
		if len(sharedResp.Conversations) != 1 || sharedResp.Conversations[0].ID != conv.ID || !sharedResp.Conversations[0].Multiplayer {
			t.Fatalf("expected Bob to see multiplayer conv %s in ?scope=shared, got %+v", conv.ID, sharedResp.Conversations)
		}

		// Bob opens the conversation directly via GET /conversations/{id}.
		wBobGet := doAuthedJSONRequest(t, srv, http.MethodGet, "/conversations/"+conv.ID, "bob@reklaim.com", nil)
		if wBobGet.Code != http.StatusOK {
			t.Fatalf("expected 200 for Bob on multiplayer conv GET, got %d: %s", wBobGet.Code, wBobGet.Body.String())
		}

		// Bob sends a message in Alice's multiplayer conversation!
		wBobChat := doAuthedJSONRequest(t, srv, http.MethodPost, "/chat", "bob@reklaim.com", map[string]any{
			"conversation_id": conv.ID,
			"message":         "Jumping in to check the CPMs",
			"model":           "anthropic/claude-opus-4.8",
		})
		if wBobChat.Code != http.StatusOK {
			t.Fatalf("expected 200 for Bob POST /chat in multiplayer conv, got %d: %s", wBobChat.Code, wBobChat.Body.String())
		}
		if !strings.Contains(wBobChat.Body.String(), `"sender_email":"bob@reklaim.com"`) {
			t.Errorf("expected SSE user.message to include Bob's sender_email, got:\n%s", wBobChat.Body.String())
		}

		// Verify the turn executed with Bob's own user identity and MCP identity, not Alice's.
		engine.mu.Lock()
		gotUser := engine.lastUserEmail
		gotSender := engine.lastSenderEmail
		gotMCPUser := engine.lastMCPUser.Email
		engine.mu.Unlock()
		if gotUser != "bob@reklaim.com" || gotSender != "bob@reklaim.com" || gotMCPUser != "bob@reklaim.com" {
			t.Errorf("turn identity mismatch: user=%q sender=%q mcpUser=%q, want bob@reklaim.com for all", gotUser, gotSender, gotMCPUser)
		}

		// Verify the persisted user message records Bob's sender_email.
		st.mu.Lock()
		hist := append([]agent.HistoryEntry(nil), st.history[conv.ID]...)
		st.mu.Unlock()
		if len(hist) == 0 {
			t.Fatalf("expected persisted history after Bob's turn")
		}
		var tc agent.TextContent
		if err := json.Unmarshal(hist[0].Content, &tc); err != nil {
			t.Fatalf("unmarshal user TextContent: %v", err)
		}
		if tc.SenderEmail != "bob@reklaim.com" {
			t.Errorf("persisted TextContent.SenderEmail = %q, want bob@reklaim.com", tc.SenderEmail)
		}

		// Bob (non-owner) cannot turn off Multiplayer mode on Alice's conversation.
		wBobDisable := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations/"+conv.ID+"/multiplayer", "bob@reklaim.com", map[string]any{
			"multiplayer": false,
		})
		if wBobDisable.Code != http.StatusForbidden {
			t.Fatalf("expected 403 when non-owner tries to disable multiplayer, got %d", wBobDisable.Code)
		}

		// Alice removes Bob from the multiplayer conversation.
		wRemoveBob := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations/"+conv.ID+"/multiplayer", "alice@reklaim.com", map[string]any{
			"remove": []string{"bob@reklaim.com"},
		})
		if wRemoveBob.Code != http.StatusOK {
			t.Fatalf("remove Bob status = %d: %s", wRemoveBob.Code, wRemoveBob.Body.String())
		}

		// Bob can no longer view or send messages to the conversation.
		wBobAfterRemove := doAuthedJSONRequest(t, srv, http.MethodGet, "/conversations/"+conv.ID, "bob@reklaim.com", nil)
		if wBobAfterRemove.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for Bob after removal, got %d", wBobAfterRemove.Code)
		}
		wBobChatAfterRemove := doAuthedJSONRequest(t, srv, http.MethodPost, "/chat", "bob@reklaim.com", map[string]any{
			"conversation_id": conv.ID,
			"message":         "still here?",
		})
		if wBobChatAfterRemove.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for Bob POST /chat after removal, got %d", wBobChatAfterRemove.Code)
		}

		// Alice adds Bob back at will.
		wAddBob := doAuthedJSONRequest(t, srv, http.MethodPost, "/conversations/"+conv.ID+"/multiplayer", "alice@reklaim.com", map[string]any{
			"add": []string{"bob@reklaim.com"},
		})
		if wAddBob.Code != http.StatusOK {
			t.Fatalf("add Bob back status = %d: %s", wAddBob.Code, wAddBob.Body.String())
		}
		wBobRestored := doAuthedJSONRequest(t, srv, http.MethodGet, "/conversations/"+conv.ID, "bob@reklaim.com", nil)
		if wBobRestored.Code != http.StatusOK {
			t.Fatalf("expected 200 for Bob after re-adding, got %d", wBobRestored.Code)
		}
	})
}
