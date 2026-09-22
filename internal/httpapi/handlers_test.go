package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
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
	st, err := store.Open(dsn, store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.TruncateAllForTest(context.Background()); err != nil {
		_ = st.Close()
		t.Fatalf("truncate: %v", err)
	}
	cfg := &config.Config{
		SharedToken:     "tok",
		PersonaDefault:  "victoria",
		ConversationTTL: 14,
		UnpinnedCap:     50,
	}
	srv := &Server{
		cfg:         cfg,
		store:       st,
		sharedToken: cfg.SharedToken,
		inflight:    make(map[string]inflightEntry),
		// New() wires this; the struct literal must too, or every /stream
		// reattach tallies into a nil counter (inc is nil-safe, so the miss is
		// silent) and reconnect-outcome assertions see an empty map.
		sseReconnects: newReconnectCounter(),
		// Handler tests aren't about the scoped-tier gate — admit every
		// authenticated user so fixtures needn't provision each email.
		// Cross-user isolation is still exercised at the handler level
		// (a different user reaches the handler and gets 404). The gate
		// itself is covered by membership_test against the real store.
		isMember: allowAllMembers,
	}
	t.Cleanup(func() { stopServerFixture(t, srv, st) })
	return srv
}

// allowAllMembers is the test override for Server.isMember: every
// authenticated email is treated as a chat member. membership_test sets
// isMember back to nil to exercise the real store.IsUser path.
func allowAllMembers(context.Context, string) (bool, error) { return true, nil }

// concreteStore returns the Postgres store behind a DB-backed fixture. Server
// holds the chatStore interface (so the always-on tests can inject an in-memory
// fake), but serverFixture/mockServer always wire a real *store.Store, so the
// DB-gated tests that need concrete-only methods (CreateUser,
// InsertTurnJournal) recover it through this assertion.
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

// TestConversationCreate_Seed proves POST /conversations with a seed persists
// one user text message WITHOUT running a turn (the "Discuss this run" bridge,
// docs/DISCUSS-RUN.md), and that an oversized seed is clamped keeping the tail.
func TestConversationCreate_Seed(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/conversations",
		map[string]string{"title": "Discuss run: nightly", "persona": "victoria", "seed": "RUN DIGEST BODY"}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create with seed: %d body=%s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}

	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "u@x.com")
	var getResp struct {
		History []struct {
			Role    string          `json:"role"`
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		} `json:"history"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if len(getResp.History) != 1 {
		t.Fatalf("seeded conversation: want exactly 1 history entry, got %d", len(getResp.History))
	}
	e := getResp.History[0]
	if e.Role != "user" || e.Type != "text" {
		t.Errorf("seed entry: want user/text, got %s/%s", e.Role, e.Type)
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(e.Content, &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if content.Text != "RUN DIGEST BODY" {
		t.Errorf("seed content: got %q", content.Text)
	}

	// Empty/whitespace seed: no message appended (plain create unchanged).
	w = do(t, h, http.MethodPost, "/conversations",
		map[string]string{"title": "plain", "persona": "victoria", "seed": "   "}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create without seed: %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &conv)
	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "u@x.com")
	if err := json.Unmarshal(w.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if len(getResp.History) != 0 {
		t.Errorf("whitespace seed: want empty history, got %d entries", len(getResp.History))
	}

	// Oversized seed: clamped to the TAIL (the outcome end of a transcript),
	// with the truncation marker prefix.
	huge := strings.Repeat("x", seedMaxChars) + "THE TAIL"
	w = do(t, h, http.MethodPost, "/conversations",
		map[string]string{"title": "big", "persona": "victoria", "seed": huge}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create with huge seed: %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &conv)
	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "u@x.com")
	if err := json.Unmarshal(w.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if len(getResp.History) != 1 {
		t.Fatalf("huge seed: want 1 entry, got %d", len(getResp.History))
	}
	if err := json.Unmarshal(getResp.History[0].Content, &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if !strings.HasPrefix(content.Text, "[…seed truncated…]") {
		t.Error("huge seed: missing truncation marker")
	}
	if !strings.HasSuffix(content.Text, "THE TAIL") {
		t.Error("huge seed: clamp must keep the tail")
	}
	if len(content.Text) > seedMaxChars+64 {
		t.Errorf("huge seed: still %d chars after clamp", len(content.Text))
	}
}

// TestConversationRename_LocksTitle proves POST /conversations/{id}/rename sets
// title_locked (#302) so the background auto-titler can't later clobber the
// user's chosen name.
func TestConversationRename_LocksTitle(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/conversations",
		map[string]string{"title": "auto", "persona": "victoria"}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d", w.Code)
	}
	var conv store.Conversation
	_ = json.Unmarshal(w.Body.Bytes(), &conv)

	if w = do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/rename",
		map[string]string{"title": "My Chosen Name"}, "u@x.com"); w.Code != http.StatusOK {
		t.Fatalf("rename: %d body=%s", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodGet, "/conversations/"+conv.ID, nil, "u@x.com")
	var getResp struct {
		Conversation store.Conversation `json:"conversation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if getResp.Conversation.Title != "My Chosen Name" || !getResp.Conversation.TitleLocked {
		t.Errorf("after rename: title=%q locked=%v (want locked)", getResp.Conversation.Title, getResp.Conversation.TitleLocked)
	}

	// The auto-titler path is now refused for this conversation.
	if err := s.store.UpdateTitle(context.Background(), "u@x.com", conv.ID, "robot"); !errors.Is(err, store.ErrTitleLocked) {
		t.Errorf("auto-title after lock: want ErrTitleLocked, got %v", err)
	}
}

// TestConversationArchiveRoute exercises POST /conversations/{id}/archive plus
// the GET ?archived=true filter (#282): archiving hides the conversation from
// the default list, unpins it, and surfaces it in the archived list;
// unarchiving restores it; a foreign user cannot archive it.
func TestConversationArchiveRoute(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/conversations",
		map[string]string{"title": "archive me", "persona": "victoria"}, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d body=%s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Pin it first, to prove archiving clears the pin.
	if w = do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/pin",
		map[string]bool{"pinned": true}, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("pin: %d", w.Code)
	}

	// A foreign user cannot archive it.
	if w = do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/archive",
		map[string]bool{"archived": true}, "intruder@x.com"); w.Code == http.StatusNoContent {
		t.Error("foreign archive should not succeed")
	}

	// Owner archives.
	if w = do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/archive",
		map[string]bool{"archived": true}, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("archive: %d body=%s", w.Code, w.Body.String())
	}

	countList := func(query, user string) (int, bool) {
		t.Helper()
		w := do(t, h, http.MethodGet, "/conversations"+query, nil, user)
		if w.Code != http.StatusOK {
			t.Fatalf("list%s: %d", query, w.Code)
		}
		var resp struct {
			Conversations []store.Conversation `json:"conversations"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		pinned := false
		for _, c := range resp.Conversations {
			if c.Pinned {
				pinned = true
			}
		}
		return len(resp.Conversations), pinned
	}

	// Gone from the default list, present (and unpinned) in the archived list.
	if n, _ := countList("", "u@x.com"); n != 0 {
		t.Errorf("archived conversation still in default list: %d", n)
	}
	if n, pinned := countList("?archived=true", "u@x.com"); n != 1 || pinned {
		t.Errorf("archived list: n=%d pinned=%v (want 1, unpinned)", n, pinned)
	}

	// Unarchive restores it to the default list.
	if w = do(t, h, http.MethodPost, "/conversations/"+conv.ID+"/archive",
		map[string]bool{"archived": false}, "u@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("unarchive: %d", w.Code)
	}
	if n, _ := countList("", "u@x.com"); n != 1 {
		t.Errorf("unarchived conversation not back in default list: %d", n)
	}
	if n, _ := countList("?archived=true", "u@x.com"); n != 0 {
		t.Errorf("archived list should be empty after unarchive: %d", n)
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

// With no operator allow-list, /server-config advertises the LIVE model tiers
// as the lockdown list — default first — so the lockdown picker leads with the
// same model a regular chat starts on, and an admin tier override shows up in
// lockdown on the next fetch without a restart.
func TestServerConfig_LockdownListDefaultsToLiveTiers(t *testing.T) {
	t.Cleanup(func() {
		agentcore.SetDefaultModel("")
		agentcore.SetAdvancedModel("")
	})
	agentcore.SetDefaultModel("")
	agentcore.SetAdvancedModel("")

	s := serverFixture(t)
	s.cfg.SandboxImage = "ghcr.io/x/y:1"
	s.cfg.LockdownAllowedModels = nil

	get := func() []string {
		w := do(t, s.Routes(), http.MethodGet, "/server-config", nil, "alice@x.com")
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
		}
		var resp serverConfigResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return resp.LockdownAllowedModels
	}
	want := []string{agentcore.DefaultCoreModel, agentcore.DefaultMaxModel}
	if got := get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lockdown_allowed_models = %v, want the compiled-in tiers %v", got, want)
	}
	agentcore.SetDefaultModel("acme/frontier-1")
	want = []string{"acme/frontier-1", agentcore.DefaultMaxModel}
	if got := get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after an admin default override lockdown_allowed_models = %v, want %v", got, want)
	}
}

// Compact is a user-visible action that used to be the one path where a
// delisted lockdown model still 400ed: the web posts the conversation's stale
// stored slug, and the summarize guard rejected it. Now that echo is run on
// the lockdown default, so the request proceeds past the lockdown guard (here
// to the handler's own "no history" 400) — WITHOUT persisting: the
// conversation migrates when its next turn launches, which is also when the
// client is told.
func TestSummarize_LockdownDelistedModelRunsOnTheDefault(t *testing.T) {
	s := serverFixture(t)
	s.cfg.SandboxImage = "ghcr.io/x/y:1"
	s.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "generic", "a/b", true)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.LockdownAllowedModels = []string{"c/d"} // a/b delisted after creation

	w := do(t, s.Routes(), http.MethodPost, "/conversations/"+conv.ID+"/summarize",
		map[string]string{"model": "a/b"}, user)
	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "not allowed in lockdown") {
		t.Fatalf("summarize still rejects the stale echoed model: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no history to summarize") {
		t.Fatalf("expected to reach the handler's own no-history check, got %d %s", w.Code, w.Body.String())
	}
	got, err := s.store.Get(t.Context(), user, conv.ID)
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Model != "a/b" {
		t.Fatalf("Compact must not persist the migration (the launching turn does, and tells the client): model=%q", got.Model)
	}

	// A genuinely different disallowed request is still refused.
	w = do(t, s.Routes(), http.MethodPost, "/conversations/"+conv.ID+"/summarize",
		map[string]string{"model": "evil/unvetted"}, user)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not allowed in lockdown") {
		t.Fatalf("different disallowed model should be refused, got %d %s", w.Code, w.Body.String())
	}
}

// The migration persists the replacement and announces it on the `conversation`
// event — but emitting an event is not proof the browser received it, and the
// socket dying in that instant is the very situation the migration exists for.
// The next submission then echoes the pre-migration slug. Refusing it would
// leave the conversation at 400 until the user reloaded, so the echo is
// recognised; a genuinely different disallowed slug is still refused.
func TestLockdownMigration_StaleEchoIsRecognisedAfterTheEventIsMissed(t *testing.T) {
	s := serverFixture(t)
	s.cfg.SandboxImage = "ghcr.io/x/y:1"
	s.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "generic", "a/b", true)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.LockdownAllowedModels = []string{"c/d"} // a/b delisted

	// The turn launches and migrates; assume the client never saw the event.
	if err := s.reconcileLockdownModelCtx(t.Context(), user, conv); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if conv.Model != "c/d" {
		t.Fatalf("migration did not run: %q", conv.Model)
	}

	reload := func() *store.Conversation {
		got, gerr := s.store.Get(t.Context(), user, conv.ID)
		if gerr != nil || got == nil {
			t.Fatalf("reload: %v", gerr)
		}
		return got
	}

	// The stale echo: accepted, and it does not drag the conversation back.
	fresh := reload()
	w := httptest.NewRecorder()
	if !s.applyTurnModelOverride(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat", nil), user, fresh, "a/b") {
		t.Fatalf("the pre-migration echo must be accepted, got %d %s", w.Code, w.Body.String())
	}
	if fresh.Model != "c/d" {
		t.Fatalf("the echo must not change the stored model, got %q", fresh.Model)
	}

	// A different disallowed slug is still a deliberate, refusable request.
	fresh = reload()
	w = httptest.NewRecorder()
	if s.applyTurnModelOverride(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat", nil), user, fresh, "evil/unvetted") {
		t.Fatalf("a different disallowed model must still be refused")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}

	// Another tab acknowledging the migration says nothing about what a third
	// one is still holding, so the record survives an allowed selection.
	fresh = reload()
	w = httptest.NewRecorder()
	if !s.applyTurnModelOverride(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat", nil), user, fresh, "c/d") {
		t.Fatalf("an allowed model must be accepted: %d %s", w.Code, w.Body.String())
	}
	fresh = reload()
	w = httptest.NewRecorder()
	if !s.applyTurnModelOverride(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat", nil), user, fresh, "a/b") {
		t.Fatalf("a second stale client must still be recognised: %d %s", w.Code, w.Body.String())
	}

	// Once the operator puts that slug back on the allow-list it stops being
	// an echo: it is an ordinary selection and must be honoured.
	s.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
	fresh = reload()
	w = httptest.NewRecorder()
	if !s.applyTurnModelOverride(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat", nil), user, fresh, "a/b") {
		t.Fatalf("a re-allowed model must be accepted: %d %s", w.Code, w.Body.String())
	}
	if got := reload(); got.Model != "a/b" {
		t.Fatalf("a re-allowed model must actually be selected, stored=%q", got.Model)
	}
}

// Compact posts the client's selected model. After a migration the client
// never saw, that slug differs from the stored model, so the pre-migration
// equality test could not recognise it and Compact 400ed.
func TestSummarize_LockdownStaleEchoAfterMigration(t *testing.T) {
	s := serverFixture(t)
	s.cfg.SandboxImage = "ghcr.io/x/y:1"
	s.cfg.LockdownAllowedModels = []string{"a/b", "c/d"}
	const user = "alice@x.com"
	conv, err := s.store.CreateConversation(t.Context(), user, "q", "generic", "a/b", true)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.LockdownAllowedModels = []string{"c/d"}
	if err := s.reconcileLockdownModelCtx(t.Context(), user, conv); err != nil {
		t.Fatalf("migration: %v", err)
	}

	w := do(t, s.Routes(), http.MethodPost, "/conversations/"+conv.ID+"/summarize",
		map[string]string{"model": "a/b"}, user)
	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "not allowed in lockdown") {
		t.Fatalf("Compact must recognise the post-migration echo: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no history to summarize") {
		t.Fatalf("expected the handler's own no-history check, got %d %s", w.Code, w.Body.String())
	}
}

// The memo is bounded: the oldest record is evicted, never the newest.
func TestMigratedModelMemo_EvictsOldestFirst(t *testing.T) {
	var memo migratedModelMemo
	for i := range migratedModelMemoCap + 10 {
		memo.note(fmt.Sprintf("conv-%d", i), fmt.Sprintf("old/model-%d", i))
	}
	if memo.matches("conv-0", "old/model-0") {
		t.Errorf("the oldest record should have been evicted")
	}
	newest := migratedModelMemoCap + 9
	if !memo.matches(fmt.Sprintf("conv-%d", newest), fmt.Sprintf("old/model-%d", newest)) {
		t.Errorf("the newest record must survive the cap")
	}
	if got := len(memo.from); got > migratedModelMemoCap {
		t.Errorf("memo grew past its cap: %d", got)
	}
	// A second migration replaces the record rather than stacking one.
	memo.note("conv-x", "old/one")
	memo.note("conv-x", "old/two")
	if memo.matches("conv-x", "old/one") || !memo.matches("conv-x", "old/two") {
		t.Errorf("only the most recent pre-migration slug is remembered")
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

// GET /personas is read-only: any other verb is a 405 before the persona
// roster is consulted (it used to answer 200 to POST/PUT/DELETE). DB- and
// engine-independent: the method check runs first, so a bare Server suffices.
func TestPersonas_MethodNotAllowed(t *testing.T) {
	s := &Server{}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/personas", nil)
		w := httptest.NewRecorder()
		s.listPersonas(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /personas: status %d, want 405", method, w.Code)
		}
	}
}
