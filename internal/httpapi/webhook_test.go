package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
)

const (
	webhookSecretEnv = "FLEET_TEST_WEBHOOK_SECRET"
	slackSecretEnv   = "FLEET_TEST_SLACK_SECRET"
	webhookOwner     = "owner@x.com"
)

// newWebhookServer builds a chat Server wired with one HMAC trigger ("gh") and
// one Slack trigger ("slack"), both owned by webhookOwner. The signing secrets
// are set in the process env under the trigger's declared env-var names.
func newWebhookServer(t *testing.T, engine turnEngine, st chatStore) *Server {
	t.Helper()
	t.Setenv(webhookSecretEnv, "hmac-signing-secret")
	t.Setenv(slackSecretEnv, "slack-signing-secret")
	srv := newDefaultChatServer(t, engine, st)
	srv.clientConfig = &clientconfig.Bundle{
		WebhookTriggers: []clientconfig.WebhookTriggerDef{
			{
				Slug:           "gh",
				HMACSecretEnv:  webhookSecretEnv,
				Persona:        "code-reviewer",
				Model:          "anthropic/claude-opus-4.8",
				PromptTemplate: "PR: {{.payload.title}}",
				NotifyUser:     webhookOwner,
			},
			{
				Slug:           "slack",
				TokenSecretEnv: slackSecretEnv,
				Persona:        "assistant",
				PromptTemplate: "Message: {{.payload.event.text}}",
				NotifyUser:     webhookOwner,
			},
		},
	}
	return srv
}

func hmacSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func slackSign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, srv *Server, slug string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/"+slug, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, req)
	return w
}

func TestWebhookValidHMACCreatesConversationAndRunsTurn(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"Fix the widget"}`)
	w := postWebhook(t, srv, "gh", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body),
	})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if resp.ConversationID == "" {
		t.Fatalf("response missing conversation_id: %s", w.Body.String())
	}

	// Conversation is created under the trigger's notify_user with its persona.
	st.mu.Lock()
	created := st.created
	conv := st.convs[resp.ConversationID]
	st.mu.Unlock()
	if created != 1 {
		t.Errorf("CreateConversation calls = %d, want 1", created)
	}
	if conv == nil || conv.UserEmail != webhookOwner || conv.Persona != "code-reviewer" {
		t.Errorf("conversation = %+v, want owner=%s persona=code-reviewer", conv, webhookOwner)
	}

	// The turn runs fire-and-forget in a goroutine (no SSE attach).
	eventually(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.turns == 1
	}, "webhook turn never ran")

	// The rendered prompt (from the payload) reached the engine as the user turn.
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.lastHistory) != 0 {
		t.Errorf("first turn history = %d entries, want 0 (fresh conversation)", len(engine.lastHistory))
	}
}

func TestWebhookBadSignatureRejected(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"x"}`)
	w := postWebhook(t, srv, "gh", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("WRONG-secret", body),
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.created != 0 {
		t.Errorf("bad signature created %d conversations, want 0", st.created)
	}
}

func TestWebhookMissingSignatureRejected(t *testing.T) {
	srv := newWebhookServer(t, &fakeEngine{}, newFakeChatStore())
	w := postWebhook(t, srv, "gh", []byte(`{"title":"x"}`), nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", w.Code)
	}
}

// TestWebhookUnknownSlugRejected asserts an unknown slug returns the SAME 401 as
// a bad signature — no 404 that would let a caller enumerate configured slugs.
func TestWebhookUnknownSlugRejected(t *testing.T) {
	st := newFakeChatStore()
	srv := newWebhookServer(t, &fakeEngine{}, st)

	body := []byte(`{"title":"x"}`)
	// A well-formed HMAC for the body, but under a slug that does not exist.
	w := postWebhook(t, srv, "does-not-exist", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body),
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown slug status %d, want 401 (timing-equalized, not 404)", w.Code)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.created != 0 {
		t.Errorf("unknown slug created %d conversations, want 0", st.created)
	}
}

func TestWebhookRateLimited(t *testing.T) {
	t.Setenv("FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE", "1")
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"x"}`)
	hdr := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body)}

	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusAccepted {
		t.Fatalf("first request status %d, want 202: %s", w.Code, w.Body.String())
	}
	// Second within the same minute exceeds the per-slug cap of 1.
	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status %d, want 429", w.Code)
	}
}

func TestWebhookTemplateErrorReturns500(t *testing.T) {
	st := newFakeChatStore()
	srv := newWebhookServer(t, &fakeEngine{}, st)
	// Override the "gh" trigger with a malformed template.
	srv.clientConfig.WebhookTriggers[0].PromptTemplate = "{{.payload.title" // unterminated action

	body := []byte(`{"title":"x"}`)
	w := postWebhook(t, srv, "gh", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body),
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.created != 0 {
		t.Errorf("template error created %d conversations, want 0", st.created)
	}
}

func TestWebhookInvalidJSONReturns400(t *testing.T) {
	srv := newWebhookServer(t, &fakeEngine{}, newFakeChatStore())
	body := []byte(`not json`)
	w := postWebhook(t, srv, "gh", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body),
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	srv := newWebhookServer(t, &fakeEngine{}, newFakeChatStore())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/webhooks/gh", nil)
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", w.Code)
	}
}

// TestWebhookHonorsLockdownOnly asserts that on a CHAT_LOCKDOWN_ONLY server the
// webhook-triggered conversation is created network-sealed (lockdown=true), the
// same as every human chat turn — the webhook path must not silently drop the
// operator's global seal on the untrusted-payload path.
func TestWebhookHonorsLockdownOnly(t *testing.T) {
	t.Setenv(webhookSecretEnv, "hmac-signing-secret")
	engine := &fakeEngine{}
	st := newFakeChatStore()
	cfg := &config.Config{
		SharedToken:        "tok",
		PersonaDefault:     "generic",
		ConversationTTL:    14,
		UnpinnedCap:        50,
		MockMode:           false,
		EmailAttachmentDir: t.TempDir(),
		LockdownOnly:       true,
		SandboxImage:       "fleet-sandbox:test", // makes LockdownAvailable() true
	}
	srv := New(cfg, engine, st)
	srv.isMember = allowAllMembers
	srv.clientConfig = &clientconfig.Bundle{
		WebhookTriggers: []clientconfig.WebhookTriggerDef{{
			Slug:          "gh",
			HMACSecretEnv: webhookSecretEnv,
			Persona:       "code-reviewer",
			// No model, so the lockdown model-allowlist check is skipped.
			PromptTemplate: "PR: {{.payload.title}}",
			NotifyUser:     webhookOwner,
		}},
	}

	body := []byte(`{"title":"x"}`)
	w := postWebhook(t, srv, "gh", body, map[string]string{
		"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body),
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	st.mu.Lock()
	conv := st.convs[resp.ConversationID]
	st.mu.Unlock()
	if conv == nil || !conv.Lockdown {
		t.Fatalf("conversation lockdown = %+v, want lockdown=true (CHAT_LOCKDOWN_ONLY must apply to webhooks)", conv)
	}
}

func TestWebhookSlackSignatureAndChallenge(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newWebhookServer(t, engine, st)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	// URL-verification handshake: echo the challenge, create nothing.
	challengeBody := []byte(`{"type":"url_verification","challenge":"abc123"}`)
	w := postWebhook(t, srv, "slack", challengeBody, map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         slackSign("slack-signing-secret", ts, challengeBody),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("challenge status %d, want 200: %s", w.Code, w.Body.String())
	}
	var chResp struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &chResp); err != nil || chResp.Challenge != "abc123" {
		t.Fatalf("challenge response = %s, want challenge=abc123", w.Body.String())
	}
	st.mu.Lock()
	createdAfterChallenge := st.created
	st.mu.Unlock()
	if createdAfterChallenge != 0 {
		t.Errorf("url_verification created %d conversations, want 0", createdAfterChallenge)
	}

	// A real event: valid Slack signature creates a conversation.
	eventBody := []byte(`{"event":{"text":"hello fleet"}}`)
	ts2 := strconv.FormatInt(time.Now().Unix(), 10)
	w2 := postWebhook(t, srv, "slack", eventBody, map[string]string{
		"X-Slack-Request-Timestamp": ts2,
		"X-Slack-Signature":         slackSign("slack-signing-secret", ts2, eventBody),
	})
	if w2.Code != http.StatusAccepted {
		t.Fatalf("slack event status %d, want 202: %s", w2.Code, w2.Body.String())
	}
	eventually(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.created == 1
	}, "slack event did not create a conversation")
}
