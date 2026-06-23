package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// testDSN returns the Postgres DSN for httpapi tests. It reads the canonical
// FLEET_TEST_DATABASE_URL first, falling back to the legacy
// CHAT_TEST_DATABASE_URL so existing .env files keep working during the fleet
// monorepo migration. Empty means no test database is configured.
func testDSN() string {
	if v := os.Getenv("FLEET_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("CHAT_TEST_DATABASE_URL")
}

// serverFixture wires a Server around a fresh Postgres store + nil Manager.
// Handlers that don't invoke the agent (list/create/get/delete/pin) work
// with a nil manager; /chat is covered by live smoke tests, not this suite.
//
// Skips when the test DSN (see testDSN) is unset so laptops without a running
// Postgres still pass `go test ./...`.
func serverFixture(t *testing.T) *Server {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL is not set — skipping Postgres-backed test")
	}
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.TruncateAllForTest(context.Background()); err != nil {
		_ = st.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		SharedToken:     "tok",
		PersonaDefault:  "victoria",
		ConversationTTL: 14,
		UnpinnedCap:     50,
	}
	return &Server{
		cfg:         cfg,
		store:       st,
		sharedToken: cfg.SharedToken,
		inflight:    make(map[string]inflightEntry),
		// Handler tests aren't about the scoped-tier gate — admit every
		// authenticated user so fixtures needn't provision each email.
		// Cross-user isolation is still exercised at the handler level
		// (a different user reaches the handler and gets 404). The gate
		// itself is covered by membership_test against the real store.
		isMember: allowAllMembers,
	}
}

// allowAllMembers is the test override for Server.isMember: every
// authenticated email is treated as a chat member. membership_test sets
// isMember back to nil to exercise the real store.IsUser path.
func allowAllMembers(context.Context, string) (bool, error) { return true, nil }

// concreteStore returns the Postgres store behind a DB-backed fixture. Server
// holds the chatStore interface (so the always-on tests can inject an in-memory
// fake), but serverFixture/mockServer always wire a real *store.Store, so the
// DB-gated tests that need concrete-only methods (CreateUser, MarkRunningTurns-
// Errored) recover it through this assertion.
func (s *Server) concreteStore(t *testing.T) *store.Store {
	t.Helper()
	st, ok := s.store.(*store.Store)
	if !ok {
		t.Fatalf("fixture store is %T, not *store.Store", s.store)
	}
	return st
}

// seedUser provisions a chat user so requests authenticating as that email
// clear membershipMiddleware. Idempotent: a duplicate insert is ignored so
// callers needn't track which emails the fixture already seeded.
func seedUser(t *testing.T, st *store.Store, email string) {
	t.Helper()
	if _, err := st.CreateUser(context.Background(), email, "test-password-123"); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("seed user %s: %v", email, err)
	}
}

func do(t *testing.T, h http.Handler, method, path string, body any, user string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, &buf)
	req.Header.Set("X-Chat-Server-Token", "tok")
	if user != "" {
		req.Header.Set("X-User-Email", user)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHealthz_NoAuth(t *testing.T) {
	s := serverFixture(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("healthz: %d %q", w.Code, w.Body.String())
	}
}

func TestConversationsLifecycle(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	// Empty list at first.
	w := do(t, h, http.MethodGet, "/conversations", nil, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", w.Code, w.Body.String())
	}
	var listResp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if listResp["conversations"] != nil {
		// the JSON encoder may emit null — that's fine; just make sure it's
		// not some pre-existing row.
		if arr, ok := listResp["conversations"].([]any); ok && len(arr) != 0 {
			t.Errorf("expected empty conversations: %v", listResp["conversations"])
		}
	}

	// Create.
	w = do(t, h, http.MethodPost, "/conversations",
		map[string]string{"title": "first", "persona": "generic"}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d body=%s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if conv.ID == "" || conv.Persona != "generic" || conv.Title != "first" {
		t.Errorf("created: %+v", conv)
	}

	// Pin.
	w = do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/pin",
		map[string]bool{"pinned": true}, "u@x.com")
	if w.Code != http.StatusNoContent {
		t.Errorf("pin: %d body=%s", w.Code, w.Body.String())
	}

	// Get and confirm pinned.
	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	var getResp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &getResp)
	c := getResp["conversation"].(map[string]any)
	if pinned, _ := c["pinned"].(bool); !pinned {
		t.Errorf("conversation not pinned: %+v", c)
	}

	// Cross-user access is blocked.
	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "other@x.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-user get: want 404 got %d", w.Code)
	}

	// Delete (owner).
	w = do(t, h, http.MethodDelete, "/conversations/"+conv.ID, nil, "u@x.com")
	if w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}

	// Gone.
	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "u@x.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("post-delete get: want 404 got %d", w.Code)
	}
}

func TestConversations_AuthEnforced(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	// No auth headers → 403.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/conversations", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("unauthenticated: %d", w.Code)
	}
}

// ensureStoreReady is a no-op sanity check that the fixture wiring is
// correct; used as a compile-time check that package imports resolve.
func TestFixtureReady(t *testing.T) {
	_ = context.Background()
	s := serverFixture(t)
	if s.store == nil || s.cfg == nil {
		t.Fatal("fixture not wired")
	}
}

// TestServerConfig_LockdownAvailability exercises the /server-config
// capability flag: the frontend reads it to decide whether to render
// the lockdown affordance, and we promise three states (unavailable /
// available-as-option / available-and-forced).
func TestServerConfig_LockdownAvailability(t *testing.T) {
	cases := []struct {
		name              string
		image             string
		only              bool
		wantAvailable     bool
		wantOnly          bool
		wantAllowedNonNil bool
	}{
		{"no image: unavailable", "", false, false, false, false},
		{"image set, only off: available, optional", "ghcr.io/x/y:1", false, true, false, true},
		{"image set, only on: available + forced", "ghcr.io/x/y:1", true, true, true, true},
		// LockdownOnly is silently dropped at Load() when image is
		// unset, but if we end up with only=true and image="" by
		// other paths, the response should still mark unavailable
		// (frontend hides the UI either way).
		{"only without image (defensive)", "", true, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := serverFixture(t)
			s.cfg.SandboxImage = tc.image
			s.cfg.LockdownOnly = tc.only
			s.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
			w := do(t, s.Routes(), http.MethodGet, "/server-config", nil, "alice@x.com")
			if w.Code != http.StatusOK {
				t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
			}
			var resp serverConfigResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.LockdownAvailable != tc.wantAvailable {
				t.Errorf("LockdownAvailable = %v, want %v", resp.LockdownAvailable, tc.wantAvailable)
			}
			if resp.LockdownOnly != tc.wantOnly {
				t.Errorf("LockdownOnly = %v, want %v", resp.LockdownOnly, tc.wantOnly)
			}
			gotNonNil := len(resp.LockdownAllowedModels) > 0
			if gotNonNil != tc.wantAllowedNonNil {
				t.Errorf("LockdownAllowedModels non-empty = %v, want %v (got=%v)", gotNonNil, tc.wantAllowedNonNil, resp.LockdownAllowedModels)
			}
		})
	}
}

// TestCreateConversation_Lockdown covers the conversation-create
// endpoint's lockdown handling: rejection when the feature is
// unavailable, model allow-list enforcement, and the LockdownOnly
// force-flag.
func TestCreateConversation_Lockdown(t *testing.T) {
	t.Run("rejects lockdown when unavailable", func(t *testing.T) {
		s := serverFixture(t)
		s.cfg.SandboxImage = "" // lockdown unavailable
		body := map[string]any{"title": "t", "lockdown": true}
		w := do(t, s.Routes(), http.MethodPost, "/conversations", body, "alice@x.com")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("rejects out-of-list model in lockdown", func(t *testing.T) {
		s := serverFixture(t)
		s.cfg.SandboxImage = "ghcr.io/x/y:1"
		s.cfg.LockdownAllowedModels = []string{"a/b"}
		body := map[string]any{"title": "t", "lockdown": true, "model": "openai/gpt-5"}
		w := do(t, s.Routes(), http.MethodPost, "/conversations", body, "alice@x.com")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("LockdownOnly force-flags the conversation", func(t *testing.T) {
		s := serverFixture(t)
		s.cfg.SandboxImage = "ghcr.io/x/y:1"
		s.cfg.LockdownOnly = true
		s.cfg.LockdownAllowedModels = []string{"a/b"}
		// Body intentionally omits lockdown:true — the operator-side
		// LockdownOnly flag should add it server-side.
		body := map[string]any{"title": "t"}
		w := do(t, s.Routes(), http.MethodPost, "/conversations", body, "alice@x.com")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
		var conv struct {
			ID       string `json:"id"`
			Lockdown bool   `json:"lockdown"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !conv.Lockdown {
			t.Errorf("expected lockdown=true (forced by LockdownOnly), got false")
		}
	})

	t.Run("normal mode preserves explicit lockdown=false", func(t *testing.T) {
		s := serverFixture(t)
		s.cfg.SandboxImage = "ghcr.io/x/y:1"
		// LockdownOnly false → user choice respected
		body := map[string]any{"title": "t"}
		w := do(t, s.Routes(), http.MethodPost, "/conversations", body, "alice@x.com")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
		var conv struct {
			ID       string `json:"id"`
			Lockdown bool   `json:"lockdown"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if conv.Lockdown {
			t.Errorf("expected lockdown=false (no opt-in), got true")
		}
	})
}
