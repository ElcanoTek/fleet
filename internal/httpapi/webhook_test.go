package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/webhooks"
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

// The webhook delivery claim, modelled in memory (the SQL is covered by the
// store's own tests against Postgres).
type fakeWebhookDelivery struct {
	conv, token string
	completed   bool
}

func (s *fakeChatStore) ClaimWebhookDelivery(_ context.Context, slug, key string) (store.WebhookClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.webhookDeliveries == nil {
		s.webhookDeliveries = map[string]*fakeWebhookDelivery{}
	}
	k := slug + "\x00" + key
	d, ok := s.webhookDeliveries[k]
	if ok && (d.completed || !s.webhookClaimsStale) {
		return store.WebhookClaim{ConversationID: d.conv, Completed: d.completed}, nil
	}
	s.webhookTokens++
	tok := fmt.Sprintf("tok-%d", s.webhookTokens)
	next := &fakeWebhookDelivery{token: tok}
	if ok {
		next.conv = d.conv // a takeover keeps a bound conversation for reuse
	}
	s.webhookDeliveries[k] = next
	return store.WebhookClaim{Claimed: true, Token: tok, ConversationID: next.conv}, nil
}

func (s *fakeChatStore) CreateWebhookConversation(ctx context.Context, slug, key, token, prevConvID, userEmail, title, persona, model string, lockdown bool) (*store.Conversation, error) {
	if s.beforeWebhookBind != nil {
		hook := s.beforeWebhookBind
		s.beforeWebhookBind = nil
		hook()
	}
	s.mu.Lock()
	if err := s.webhookBindErr; err != nil {
		s.webhookBindErr = nil
		s.mu.Unlock()
		return nil, err
	}
	d := s.webhookDeliveries[slug+"\x00"+key]
	if d == nil || d.token != token || d.conv != prevConvID || d.completed {
		s.mu.Unlock()
		return nil, store.ErrWebhookClaimLost
	}
	s.mu.Unlock()
	conv, err := s.CreateConversation(ctx, userEmail, title, persona, model, lockdown)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	d.conv = conv.ID
	s.mu.Unlock()
	return conv, nil
}

func (s *fakeChatStore) CommitWebhookUserMessage(ctx context.Context, slug, key, token, convID, turnID string, entry agent.HistoryEntry) (int64, error) {
	s.mu.Lock()
	if err := s.webhookCommitErr; err != nil {
		s.webhookCommitErr = nil
		s.mu.Unlock()
		return 0, err
	}
	d := s.webhookDeliveries[slug+"\x00"+key]
	if d == nil || d.token != token || d.conv != convID || d.completed {
		s.mu.Unlock()
		return 0, store.ErrWebhookClaimLost
	}
	d.completed = true
	s.mu.Unlock()
	return s.CommitUserMessage(ctx, convID, turnID, entry)
}

// waitWebhookDeliveriesCompleted waits until every bound delivery in the fake
// is complete — the turn goroutine commits its user message (and with it the
// completion) shortly after the 202, and only then is a repeat a duplicate.
func waitWebhookDeliveriesCompleted(t *testing.T, st *fakeChatStore) {
	t.Helper()
	eventually(t, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		for _, d := range st.webhookDeliveries {
			if d.conv != "" && !d.completed {
				return false
			}
		}
		return true
	}, "a webhook turn never committed its user message")
}

// webhookDeliveryCompleted reports whether the fake's delivery record is
// complete (the turn's user message committed).
func webhookDeliveryCompleted(st *fakeChatStore, key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	d := st.webhookDeliveries["gh\x00"+key]
	return d != nil && d.completed
}

func (s *fakeChatStore) ReleaseWebhookDelivery(_ context.Context, slug, key, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.webhookDeliveries[slug+"\x00"+key]; d != nil && d.token == token && !d.completed {
		delete(s.webhookDeliveries, slug+"\x00"+key)
	}
	return nil
}

func (s *fakeChatStore) AbandonWebhookConversation(_ context.Context, slug, key, token, convID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.webhookDeliveries[slug+"\x00"+key]; d != nil && d.token == token && d.conv == convID && !d.completed {
		delete(s.webhookDeliveries, slug+"\x00"+key)
		if len(s.history[convID]) == 0 { // only while empty
			delete(s.convs, convID)
		}
	}
	return nil
}

func (s *fakeChatStore) SweepWebhookDeliveries(context.Context, time.Duration) (int, error) {
	return 0, nil
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

// TestWebhookSignatureWorkEqualized (#572) asserts STRUCTURALLY — via the
// webhooks BodyHMACComputations test seam, not wall-clock timing — that every
// rejected request performs the same signature work over the body, regardless
// of slug existence, trigger verifier type (Slack vs HMAC), or which signature
// headers the caller chose to send/omit. The attack this pins down: a
// Slack-shaped probe (valid timestamp, garbage signature, NO default HMAC
// header) used to do a full-body HMAC only when the slug named a real Slack
// trigger, so response timing grew with body size on hits but not misses —
// leaking slug existence. Same asymmetry for a custom-hmac_header trigger.
func TestWebhookSignatureWorkEqualized(t *testing.T) {
	srv := newWebhookServer(t, &fakeEngine{}, newFakeChatStore())
	// Add a GitHub-style trigger with a NON-default signature header — the other
	// verifier shape whose miss path used to skip the HMAC entirely.
	srv.clientConfig.WebhookTriggers = append(srv.clientConfig.WebhookTriggers, clientconfig.WebhookTriggerDef{
		Slug:          "gh-custom",
		HMACSecretEnv: webhookSecretEnv,
		HMACHeader:    "X-Custom-Signature-256",
		NotifyUser:    webhookOwner,
	})

	body := []byte(`{"probe":true}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	// Slack-shaped probe: plausible timestamp, garbage signature, and the
	// default X-Hub-Signature-256 deliberately omitted.
	slackShaped := map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         "v0=" + strings.Repeat("ab", 32),
	}
	cases := []struct {
		name    string
		slug    string
		headers map[string]string
	}{
		{"slack-shaped, existing slack slug", "slack", slackShaped},
		{"slack-shaped, unknown slug", "no-such-slug", slackShaped},
		{"custom-header trigger, header omitted", "gh-custom", nil},
		{"default hmac trigger, header omitted", "gh", nil},
		{"no signature headers, unknown slug", "also-missing", nil},
	}
	counts := make([]uint64, len(cases))
	for i, tc := range cases {
		before := webhooks.BodyHMACComputations()
		if w := postWebhook(t, srv, tc.slug, body, tc.headers); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", tc.name, w.Code)
		}
		counts[i] = webhooks.BodyHMACComputations() - before
	}
	// Every shape performs exactly 2 full-body computations (one Slack-style +
	// one HMAC-SHA256, unconditionally) — equal work, no oracle.
	for i, n := range counts {
		if n != 2 {
			t.Errorf("%s: performed %d body HMACs, want 2 (constant work across hit/miss and verifier shapes)", cases[i].name, n)
		}
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
	// A second, different delivery within the same minute exceeds the
	// per-slug cap of 1. (A repeat of the first one would be answered as a
	// duplicate instead — see TestWebhookDuplicateDelivery*.)
	body2 := []byte(`{"title":"y"}`)
	hdr2 := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body2)}
	if w := postWebhook(t, srv, "gh", body2, hdr2); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status %d, want 429", w.Code)
	}
	// The throttled delivery was not accepted, so its claim was released: the
	// sender's retry is throttled again rather than answered as a duplicate.
	if w := postWebhook(t, srv, "gh", body2, hdr2); w.Code != http.StatusTooManyRequests {
		t.Fatalf("retry of a throttled delivery: status %d, want 429: %s", w.Code, w.Body.String())
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

// webhookResult decodes a webhook response body.
func webhookResult(t *testing.T, w *httptest.ResponseRecorder) (convID string, duplicate bool) {
	t.Helper()
	var resp struct {
		ConversationID string `json:"conversation_id"`
		Duplicate      bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	return resp.ConversationID, resp.Duplicate
}

// The same signed request delivered twice is one delivery: the repeat is
// answered 200 with the first conversation and starts nothing.
func TestWebhookDuplicateDeliveryStartsOneConversation(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"Fix the widget"}`)
	hdr := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body)}
	w1 := postWebhook(t, srv, "gh", body, hdr)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status %d, want 202: %s", w1.Code, w1.Body.String())
	}
	first, _ := webhookResult(t, w1)

	waitWebhookDeliveriesCompleted(t, st)
	w2 := postWebhook(t, srv, "gh", body, hdr)
	if w2.Code != http.StatusOK {
		t.Fatalf("repeat delivery: status %d, want 200: %s", w2.Code, w2.Body.String())
	}
	second, dup := webhookResult(t, w2)
	if second != first || !dup {
		t.Fatalf("repeat answered conversation %q duplicate=%v, want %q duplicate=true", second, dup, first)
	}
	eventually(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.turns == 1
	}, "first delivery's turn never ran")
	st.mu.Lock()
	created := st.created
	st.mu.Unlock()
	if created != 1 {
		t.Fatalf("CreateConversation calls = %d, want 1", created)
	}
	// Give a (wrongly) started second turn the chance to show up.
	time.Sleep(50 * time.Millisecond)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.turns != 1 {
		t.Fatalf("turns run = %d, want 1", engine.turns)
	}
}

// Deliveries are told apart by X-GitHub-Delivery: two deliveries of the same
// body under different ids are two events; the same id repeated is one, even
// though nothing else identifies it.
func TestWebhookGitHubDeliveryIDs(t *testing.T) {
	st := newFakeChatStore()
	srv := newWebhookServer(t, &fakeEngine{}, st)

	body := []byte(`{"title":"same body"}`)
	sig := hmacSig("hmac-signing-secret", body)
	send := func(id string) *httptest.ResponseRecorder {
		return postWebhook(t, srv, "gh", body, map[string]string{"X-Hub-Signature-256": sig, "X-GitHub-Delivery": id})
	}
	a := send("11111111-aaaa")
	b := send("22222222-bbbb")
	if a.Code != http.StatusAccepted || b.Code != http.StatusAccepted {
		t.Fatalf("distinct delivery ids: statuses %d, %d, want 202, 202", a.Code, b.Code)
	}
	convA, _ := webhookResult(t, a)
	convB, _ := webhookResult(t, b)
	if convA == convB {
		t.Fatalf("distinct deliveries shared conversation %q", convA)
	}
	waitWebhookDeliveriesCompleted(t, st)
	again := send("11111111-aaaa")
	if again.Code != http.StatusOK {
		t.Fatalf("redelivery: status %d, want 200: %s", again.Code, again.Body.String())
	}
	if conv, dup := webhookResult(t, again); conv != convA || !dup {
		t.Fatalf("redelivery answered %q duplicate=%v, want %q", conv, dup, convA)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.created != 2 {
		t.Fatalf("CreateConversation calls = %d, want 2", st.created)
	}
}

// A Slack retry (X-Slack-Retry-Num, a fresh timestamp and signature) of an
// accepted event is recognised by the payload's event_id; a different event
// is a new delivery.
func TestWebhookSlackRetryIsDuplicate(t *testing.T) {
	st := newFakeChatStore()
	srv := newWebhookServer(t, &fakeEngine{}, st)

	send := func(body []byte, ts string, retry string) *httptest.ResponseRecorder {
		hdr := map[string]string{
			"X-Slack-Request-Timestamp": ts,
			"X-Slack-Signature":         slackSign("slack-signing-secret", ts, body),
		}
		if retry != "" {
			hdr["X-Slack-Retry-Num"] = retry
			hdr["X-Slack-Retry-Reason"] = "http_timeout"
		}
		return postWebhook(t, srv, "slack", body, hdr)
	}
	now := time.Now().Unix()
	event := []byte(`{"event_id":"Ev0001","event":{"text":"hello fleet"}}`)
	w1 := send(event, strconv.FormatInt(now, 10), "")
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first event: status %d, want 202: %s", w1.Code, w1.Body.String())
	}
	first, _ := webhookResult(t, w1)

	waitWebhookDeliveriesCompleted(t, st)
	w2 := send(event, strconv.FormatInt(now+3, 10), "1")
	if w2.Code != http.StatusOK {
		t.Fatalf("slack retry: status %d, want 200: %s", w2.Code, w2.Body.String())
	}
	if conv, dup := webhookResult(t, w2); conv != first || !dup {
		t.Fatalf("slack retry answered %q duplicate=%v, want %q", conv, dup, first)
	}

	other := []byte(`{"event_id":"Ev0002","event":{"text":"hello fleet"}}`)
	if w3 := send(other, strconv.FormatInt(now+4, 10), ""); w3.Code != http.StatusAccepted {
		t.Fatalf("a different event: status %d, want 202: %s", w3.Code, w3.Body.String())
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.created != 2 {
		t.Fatalf("CreateConversation calls = %d, want 2", st.created)
	}
}

// A request that outlives its claim (a repeat took the stale claim over and
// rotated its token) cannot bind a conversation or drop the new owner's
// claim: only the repeat starts a turn.
func TestWebhookStaleClaimTakeoverStartsOneTurn(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	st.webhookClaimsStale = true
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"slow"}`)
	hdr := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body), "X-GitHub-Delivery": "d-1"}
	var retry *httptest.ResponseRecorder
	// The original stalls just before binding; meanwhile the sender retries.
	st.beforeWebhookBind = func() { retry = postWebhook(t, srv, "gh", body, hdr) }

	original := postWebhook(t, srv, "gh", body, hdr)
	if retry == nil || retry.Code != http.StatusAccepted {
		t.Fatalf("retry that took the claim over: %v, want 202", retry)
	}
	if original.Code != http.StatusConflict {
		t.Fatalf("original that lost its claim: status %d, want 409: %s", original.Code, original.Body.String())
	}
	retryConv, _ := webhookResult(t, retry)

	eventually(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.turns == 1
	}, "the retry's turn never ran")
	time.Sleep(50 * time.Millisecond)
	engine.mu.Lock()
	turns := engine.turns
	engine.mu.Unlock()
	st.mu.Lock()
	created, rec := st.created, st.webhookDeliveries["gh\x00"+deliveryKeyFor(t, body, hdr)]
	st.mu.Unlock()
	if turns != 1 || created != 1 {
		t.Fatalf("turns=%d conversations=%d, want 1 and 1", turns, created)
	}
	// The original's deferred release was a no-op: the record still names
	// the retry's conversation.
	if rec == nil || rec.conv != retryConv {
		t.Fatalf("delivery record = %+v, want bound to %q", rec, retryConv)
	}
}

// deliveryKeyFor computes the handler's delivery key for a request.
func deliveryKeyFor(t *testing.T, body []byte, hdr map[string]string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gh", bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	return webhookDeliveryKey(req, body, payload, clientconfig.WebhookTriggerDef{Slug: "gh"})
}

// When the conversation cannot be created and bound, no turn starts and the
// claim is given back, so the sender's retry runs exactly once.
func TestWebhookBindFailureStartsNoTurn(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	st.webhookBindErr = errors.New("database unavailable")
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"x"}`)
	hdr := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body)}
	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusInternalServerError {
		t.Fatalf("bind failure: status %d, want 500: %s", w.Code, w.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	engine.mu.Lock()
	turns := engine.turns
	engine.mu.Unlock()
	if turns != 0 {
		t.Fatalf("a turn started without a bound delivery (turns=%d)", turns)
	}

	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusAccepted {
		t.Fatalf("retry after a bind failure: status %d, want 202: %s", w.Code, w.Body.String())
	}
	waitWebhookDeliveriesCompleted(t, st)
	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusOK {
		t.Fatalf("repeat of the accepted retry: status %d, want 200: %s", w.Code, w.Body.String())
	}
	eventually(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.turns == 1
	}, "the retry's turn never ran")
	time.Sleep(50 * time.Millisecond)
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.turns != 1 {
		t.Fatalf("turns = %d, want 1", engine.turns)
	}
}

// A process that bound the delivery's conversation and then died before the
// turn row existed leaves the delivery unfinished. A repeat inside the stale
// window is told to retry; one after it takes the claim over and runs the turn
// in the conversation the dead request created — one turn, no orphan.
func TestWebhookCrashBeforeTurnRetryRunsOnce(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"crash"}`)
	hdr := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body), "X-GitHub-Delivery": "d-crash"}
	key := deliveryKeyFor(t, body, hdr)

	// The dead request got as far as the bind and its turn row, but not the
	// turn's user message.
	ctx := context.Background()
	claim, err := st.ClaimWebhookDelivery(ctx, "gh", key)
	if err != nil || !claim.Claimed {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	orphan, err := st.CreateWebhookConversation(ctx, "gh", key, claim.Token, "", webhookOwner, "t", "code-reviewer", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTurn(ctx, "dead-turn", orphan.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusConflict {
		t.Fatalf("repeat inside the window: status %d, want 409: %s", w.Code, w.Body.String())
	}

	st.mu.Lock()
	st.webhookClaimsStale = true
	st.mu.Unlock()
	w := postWebhook(t, srv, "gh", body, hdr)
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry after the window: status %d, want 202: %s", w.Code, w.Body.String())
	}
	if conv, _ := webhookResult(t, w); conv != orphan.ID {
		t.Fatalf("retry ran in %q, want the bound conversation %q", conv, orphan.ID)
	}
	eventually(t, func() bool { return webhookDeliveryCompleted(st, key) }, "the retry's user message never committed")

	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusOK {
		t.Fatalf("repeat after the input is durable: status %d, want 200: %s", w.Code, w.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	engine.mu.Lock()
	turns := engine.turns
	engine.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if turns != 1 || st.created != 1 || countUserTexts(st.history[orphan.ID]) != 1 {
		t.Fatalf("turns=%d conversations=%d user messages=%d, want 1/1/1", turns, st.created, countUserTexts(st.history[orphan.ID]))
	}
}

func countUserTexts(h []agent.HistoryEntry) int {
	n := 0
	for _, e := range h {
		if e.Role == "user" && e.Type == "text" {
			n++
		}
	}
	return n
}

// The delivery completes only with the turn's user message: if that commit
// fails, the turn runs nothing, the delivery stays retryable (in progress
// inside the stale window), and a retry after it runs the turn once in the
// same conversation.
func TestWebhookUserCommitFailureLeavesDeliveryRetryable(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	st.webhookCommitErr = errors.New("database unavailable")
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"x"}`)
	hdr := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body), "X-GitHub-Delivery": "d-commit"}
	key := deliveryKeyFor(t, body, hdr)
	w := postWebhook(t, srv, "gh", body, hdr)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status %d, want 202: %s", w.Code, w.Body.String())
	}
	first, _ := webhookResult(t, w)
	eventually(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.turns == 1
	}, "the first turn never started")
	time.Sleep(50 * time.Millisecond)
	if webhookDeliveryCompleted(st, key) {
		t.Fatal("the delivery completed although its user message did not commit")
	}
	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusConflict {
		t.Fatalf("repeat inside the window: status %d, want 409: %s", w.Code, w.Body.String())
	}

	st.mu.Lock()
	st.webhookClaimsStale = true
	st.mu.Unlock()
	w = postWebhook(t, srv, "gh", body, hdr)
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry after the window: status %d, want 202: %s", w.Code, w.Body.String())
	}
	if conv, _ := webhookResult(t, w); conv != first {
		t.Fatalf("retry ran in %q, want the bound conversation %q", conv, first)
	}
	eventually(t, func() bool { return webhookDeliveryCompleted(st, key) }, "the retry's user message never committed")
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := countUserTexts(st.history[first]); n != 1 || st.created != 1 {
		t.Fatalf("user messages=%d conversations=%d, want 1/1", n, st.created)
	}
}

// A conversation inherited through a takeover is never discarded: when the
// retry that inherited it fails before its turn starts, only the claim is
// released, and the conversation (which its owner has used meanwhile) stays.
func TestWebhookFailedRetryKeepsInheritedConversation(t *testing.T) {
	engine := &fakeEngine{}
	st := newFakeChatStore()
	st.webhookClaimsStale = true
	srv := newWebhookServer(t, engine, st)

	body := []byte(`{"title":"x"}`)
	hdr := map[string]string{"X-Hub-Signature-256": hmacSig("hmac-signing-secret", body), "X-GitHub-Delivery": "d-inherit"}
	key := deliveryKeyFor(t, body, hdr)
	ctx := context.Background()
	claim, _ := st.ClaimWebhookDelivery(ctx, "gh", key)
	inherited, err := st.CreateWebhookConversation(ctx, "gh", key, claim.Token, "", webhookOwner, "t", "code-reviewer", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// The owner wrote in it.
	if _, err := st.AppendHistory(ctx, inherited.ID, []agent.HistoryEntry{histEntryText("user", "my own notes")}); err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	st.createTurnErr = errors.New("database unavailable")
	st.mu.Unlock()
	if w := postWebhook(t, srv, "gh", body, hdr); w.Code != http.StatusInternalServerError {
		t.Fatalf("retry failing before its turn: status %d, want 500: %s", w.Code, w.Body.String())
	}
	st.mu.Lock()
	_, kept := st.convs[inherited.ID]
	st.mu.Unlock()
	if !kept {
		t.Fatal("the inherited conversation was deleted")
	}
	// The claim was released: the next retry starts afresh in a new conversation.
	w := postWebhook(t, srv, "gh", body, hdr)
	if w.Code != http.StatusAccepted {
		t.Fatalf("next retry: status %d, want 202: %s", w.Code, w.Body.String())
	}
	if conv, _ := webhookResult(t, w); conv == inherited.ID {
		t.Fatal("the next retry reused the released conversation")
	}
	eventually(t, func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return engine.turns == 1
	}, "the next retry's turn never ran")
}

func histEntryText(role, text string) agent.HistoryEntry {
	raw, _ := json.Marshal(agent.TextContent{Text: text})
	return agent.HistoryEntry{Role: role, Type: "text", Content: raw}
}
