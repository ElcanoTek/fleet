package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/ratelimit"
)

// TestConversationShare_EndToEnd drives the full #226 flow through the mux:
// owner shares → public token read works → ownership is enforced → revoke makes
// the link 404.
func TestConversationShare_EndToEnd(t *testing.T) {
	s := serverFixture(t)
	st := s.concreteStore(t)
	ctx := context.Background()

	conv, err := st.CreateConversation(ctx, "alice@x.com", "Sorting in Go", "victoria", "openai/gpt-5", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := st.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"how do I sort?"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"sort.Slice"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	h := s.Routes()

	// A non-owner cannot share alice's conversation (Get is user-scoped → 404).
	if w := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/share", nil, "mallory@x.com"); w.Code != http.StatusNotFound {
		t.Fatalf("non-owner share: got %d, want 404", w.Code)
	}

	// Owner shares → 201 + token.
	w := do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/share", nil, "alice@x.com")
	if w.Code != http.StatusCreated {
		t.Fatalf("owner share: got %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var shareResp struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &shareResp); err != nil {
		t.Fatalf("decode share response: %v", err)
	}
	if shareResp.Token == "" || shareResp.URL != "/shared/"+shareResp.Token {
		t.Fatalf("share response malformed: %+v", shareResp)
	}

	// Public read with NO user identity (just the shared secret) returns the
	// snapshot — and must NOT leak the owner email or internal id.
	pub := do(t, h, http.MethodGet, "/shared/"+shareResp.Token, nil, "")
	if pub.Code != http.StatusOK {
		t.Fatalf("public read: got %d, want 200 (body=%s)", pub.Code, pub.Body.String())
	}
	body := pub.Body.String()
	if !strings.Contains(body, "Sorting in Go") {
		t.Errorf("public snapshot missing title: %s", body)
	}
	if strings.Contains(body, "alice@x.com") || strings.Contains(body, conv.ID) {
		t.Errorf("public snapshot leaked owner email or internal id: %s", body)
	}

	// A non-owner cannot revoke alice's share (ownership pre-check → 404, not 500).
	if w := do(t, h, http.MethodDelete, "/conversations/"+conv.ID+"/share", nil, "mallory@x.com"); w.Code != http.StatusNotFound {
		t.Fatalf("non-owner revoke: got %d, want 404", w.Code)
	}

	// Revoke → the link 404s.
	if w := do(t, h, http.MethodDelete, "/conversations/"+conv.ID+"/share", nil, "alice@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204", w.Code)
	}
	// Revoke again → still 204 (idempotent; the UI calls it without checking state).
	if w := do(t, h, http.MethodDelete, "/conversations/"+conv.ID+"/share", nil, "alice@x.com"); w.Code != http.StatusNoContent {
		t.Errorf("second revoke: got %d, want 204 (idempotent)", w.Code)
	}
	if pub := do(t, h, http.MethodGet, "/shared/"+shareResp.Token, nil, ""); pub.Code != http.StatusNotFound {
		t.Errorf("read after revoke: got %d, want 404", pub.Code)
	}

	// An unknown token is 404 (indistinguishable from revoked).
	if pub := do(t, h, http.MethodGet, "/shared/never-issued-token", nil, ""); pub.Code != http.StatusNotFound {
		t.Errorf("unknown token: got %d, want 404", pub.Code)
	}
}

// TestSharedConversation_ExpiredToken404 confirms server-side expiry: a token
// whose share_expires_at is in the past returns 404 through the public route.
func TestSharedConversation_ExpiredToken404(t *testing.T) {
	s := serverFixture(t)
	st := s.concreteStore(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "alice@x.com", "expiring chat", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	past := time.Now().Add(-1 * time.Hour).Unix()
	if err := st.SetShareToken(ctx, "alice@x.com", conv.ID, "expired-token-xyz", &past); err != nil {
		t.Fatalf("SetShareToken: %v", err)
	}
	if pub := do(t, s.Routes(), http.MethodGet, "/shared/expired-token-xyz", nil, ""); pub.Code != http.StatusNotFound {
		t.Errorf("expired token: got %d, want 404", pub.Code)
	}
}

// TestSharedConversation_PerTokenRateLimit confirms the public read endpoint is
// gated by the per-token limiter (the abuse gate). Uses a tiny limiter to keep
// the test fast: the wiring, not the exact 120/min bound, is what matters.
func TestSharedConversation_PerTokenRateLimit(t *testing.T) {
	s := serverFixture(t)
	s.shareRL = ratelimit.New(3, 0) // 3/min for the test
	st := s.concreteStore(t)
	ctx := context.Background()
	conv, err := st.CreateConversation(ctx, "alice@x.com", "popular chat", "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := st.SetShareToken(ctx, "alice@x.com", conv.ID, "rl-token", nil); err != nil {
		t.Fatalf("SetShareToken: %v", err)
	}
	h := s.Routes()
	for i := 0; i < 3; i++ {
		if pub := do(t, h, http.MethodGet, "/shared/rl-token", nil, ""); pub.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, pub.Code)
		}
	}
	if pub := do(t, h, http.MethodGet, "/shared/rl-token", nil, ""); pub.Code != http.StatusTooManyRequests {
		t.Errorf("over-limit request: got %d, want 429", pub.Code)
	}
}

// TestSharedEndpoint_RequiresSharedSecret confirms the public /shared route is
// still behind the token-only gate (only the trusted Next proxy reaches it):
// a request with no shared secret is rejected before any token lookup.
func TestSharedEndpoint_RequiresSharedSecret(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/shared/anything", nil)
	// No X-Chat-Server-Token header.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("no-secret /shared: got %d, want 403", w.Code)
	}
}
