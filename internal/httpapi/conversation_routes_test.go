// Locks the conversationByID dispatch table (#1127): every (sub, method) pair
// in conversationSubroutes is walked over real HTTP through Routes() and must
// answer its handler's distinctive status — so swapping two entries, deleting
// one (silent 405), or adding one without extending this test all fail here.
// The per-branch 404 body texts the old switch diverged on, and the exact 405
// default, are asserted too.

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/store"
)

// routePairProbe describes one (sub, method) dispatch-table entry and the
// status its handler answers for a freshly created, caller-owned conversation
// with the given body. Statuses are chosen to be distinctive of the HANDLER
// (not just "not 405"): e.g. branch answers 400 for a missing branch point,
// promote-to-task 422 for an empty transcript — proving the request reached
// that handler and not a neighbor's.
type routePairProbe struct {
	sub    string
	method string
	body   any
	want   int
}

func conversationRoutePairs() []routePairProbe {
	return []routePairProbe{
		{sub: "", method: http.MethodGet, body: nil, want: http.StatusOK},
		{sub: "", method: http.MethodDelete, body: nil, want: http.StatusNoContent},
		{sub: "truncate", method: http.MethodPost, body: nil, want: http.StatusNoContent},
		{sub: "pin", method: http.MethodPost, body: map[string]bool{"pinned": true}, want: http.StatusNoContent},
		{sub: "archive", method: http.MethodPost, body: map[string]bool{"archived": true}, want: http.StatusNoContent},
		// Empty project_id = unfile: exercises the refile handler without a project fixture.
		{sub: "project", method: http.MethodPost, body: map[string]string{"project_id": ""}, want: http.StatusNoContent},
		{sub: "rename", method: http.MethodPost, body: map[string]string{"title": "renamed"}, want: http.StatusOK},
		// Empty model = "keep stored": exercises the model handler's no-op leg.
		{sub: "model", method: http.MethodPost, body: map[string]string{"model": ""}, want: http.StatusNoContent},
		{sub: "approval-timeout", method: http.MethodGet, body: nil, want: http.StatusOK},
		{sub: "approval-timeout", method: http.MethodPost, body: map[string]int{"approval_timeout_seconds": 60}, want: http.StatusNoContent},
		{sub: "thinking_config", method: http.MethodGet, body: nil, want: http.StatusOK},
		{sub: "thinking_config", method: http.MethodPut, body: map[string]any{"enabled": true, "budget_tokens": 2048}, want: http.StatusNoContent},
		{sub: "thinking_config", method: http.MethodDelete, body: nil, want: http.StatusNoContent},
		// No branch_point_message_id → the branch handler's own 400, not a 405.
		{sub: "branch", method: http.MethodPost, body: nil, want: http.StatusBadRequest},
		// Empty transcript → the synthesizers' own 422s, before any engine call.
		{sub: "promote-to-task", method: http.MethodPost, body: nil, want: http.StatusUnprocessableEntity},
		{sub: "suggest-prompt", method: http.MethodPost, body: nil, want: http.StatusUnprocessableEntity},
		{sub: "share", method: http.MethodPost, body: nil, want: http.StatusCreated},
		{sub: "share", method: http.MethodDelete, body: nil, want: http.StatusNoContent},
		// Probed on a fresh chat that is in no project, which a chat cannot be
		// shared from (ADR-0057) — so the handler answers its own 409, not the
		// dispatcher's 405. The 200 path is covered in team_sharing_http_test.
		{sub: "share-with-team", method: http.MethodPost, body: map[string]bool{"visible": true}, want: http.StatusConflict},
		// The one conversation route a non-owner may reach (ADR-0057). Probed
		// on a fresh, unshared chat, so the handler answers its own 404 —
		// distinct from the dispatcher's 405 for an unmatched pair. The 200
		// path (a teammate reading a shared chat) is covered end to end by
		// TestConversationTeamView, which needs a two-user team fixture this
		// single-owner probe deliberately does not build.
		{sub: "team-view", method: http.MethodGet, body: nil, want: http.StatusNotFound},
		{sub: "mcp-servers", method: http.MethodGet, body: nil, want: http.StatusOK},
		{sub: "mcp-servers", method: http.MethodPost, body: map[string][]string{"enabled_optional": {}}, want: http.StatusOK},
		{sub: "export", method: http.MethodGet, body: nil, want: http.StatusOK},
		{sub: "cancel", method: http.MethodPost, body: nil, want: http.StatusNoContent},
		// Conversation created with no stored model → the summarize handler's
		// own "model required" 400, before any engine call.
		{sub: "summarize", method: http.MethodPost, body: nil, want: http.StatusBadRequest},
	}
}

// newRouteProbeConversation creates a fresh conversation owned by user, so
// each probed pair runs against untouched state (DELETE, project and archive
// probes mutate theirs).
func newRouteProbeConversation(t *testing.T, h http.Handler) string {
	t.Helper()
	w := do(t, h, http.MethodPost, "/conversations",
		map[string]string{"title": "route probe", "persona": "generic"}, "router@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create conversation: %d body=%s", w.Code, w.Body.String())
	}
	var conv store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &conv); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	return conv.ID
}

func TestConversationSubrouteTable(t *testing.T) {
	s := serverFixture(t)
	// The Tools-picker pairs (mcp-servers GET/POST) read the Optional-server
	// catalog through the engine seam; the fake answers an empty catalog.
	s.agent = &fakeEngine{}
	h := s.Routes()
	const user = "router@x.com"

	pairs := conversationRoutePairs()

	// Bidirectional sync with the dispatch table: every walked pair must be a
	// table entry, and every table entry must be walked. Deleting an entry or
	// adding one without extending this test fails here before any HTTP.
	if len(pairs) != len(conversationSubroutes) {
		t.Fatalf("test walks %d pairs but conversationSubroutes has %d entries — keep them in sync",
			len(pairs), len(conversationSubroutes))
	}
	for _, p := range pairs {
		if _, ok := conversationSubroutes[conversationSubroute{sub: p.sub, method: p.method}]; !ok {
			t.Fatalf("probed pair (%q, %s) is not in conversationSubroutes", p.sub, p.method)
		}
	}

	for _, p := range pairs {
		name := p.sub
		if name == "" {
			name = "item"
		}
		t.Run(name+" "+p.method, func(t *testing.T) {
			id := newRouteProbeConversation(t, h)
			path := "/conversations/" + id
			if p.sub != "" {
				path += "/" + p.sub
			}
			w := do(t, h, p.method, path, p.body, user)
			if w.Code != p.want {
				t.Fatalf("%s %s: got %d want %d body=%q", p.method, path, w.Code, p.want, w.Body.String())
			}
		})
	}

	// The method-agnostic queue branch (not a table entry — it matches any
	// method and dispatches internally).
	t.Run("queue GET", func(t *testing.T) {
		id := newRouteProbeConversation(t, h)
		w := do(t, h, http.MethodGet, "/conversations/"+id+"/queue", nil, user)
		if w.Code != http.StatusOK {
			t.Fatalf("queue snapshot: got %d want 200 body=%q", w.Code, w.Body.String())
		}
	})
}

// TestConversationSubroute404Bodies pins the per-branch 404 responses for a
// conversation the caller does not own (here: one that does not exist — the
// ownership gate makes the two indistinguishable by design). The old switch
// diverged on the body text per branch and the split preserves each verbatim:
// five branches answer "not found", cancel answers "conversation not found",
// and the model route goes through http.NotFound ("404 page not found").
func TestConversationSubroute404Bodies(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeEngine{}
	h := s.Routes()
	const user = "router@x.com"
	missing := uuid.NewString() // valid UUID, never created

	cases := []struct {
		name     string
		method   string
		sub      string
		body     any
		wantBody string
	}{
		{name: "item GET", method: http.MethodGet, sub: "", wantBody: "not found\n"},
		{name: "approval-timeout GET", method: http.MethodGet, sub: "approval-timeout", wantBody: "not found\n"},
		{name: "thinking_config GET", method: http.MethodGet, sub: "thinking_config", wantBody: "not found\n"},
		{name: "mcp-servers GET", method: http.MethodGet, sub: "mcp-servers", wantBody: "not found\n"},
		{name: "export GET", method: http.MethodGet, sub: "export", wantBody: "not found\n"},
		{name: "cancel POST", method: http.MethodPost, sub: "cancel", wantBody: "conversation not found\n"},
		// model POST only checks ownership when a non-empty model must pass
		// the lockdown gate, and historically answers via http.NotFound.
		{name: "model POST", method: http.MethodPost, sub: "model",
			body: map[string]string{"model": "some/model"}, wantBody: "404 page not found\n"},
		// The mutators below gate ownership inside the store (0 rows affected →
		// store.ErrConversationNotFound). They used to surface that as a 500;
		// writeConversationMutationError maps the sentinel to 404 for all of
		// them so a stale tab is never told the server is broken.
		{name: "item DELETE", method: http.MethodDelete, sub: "", wantBody: "conversation not found\n"},
		{name: "pin POST", method: http.MethodPost, sub: "pin",
			body: map[string]bool{"pinned": true}, wantBody: "conversation not found\n"},
		{name: "archive POST", method: http.MethodPost, sub: "archive",
			body: map[string]bool{"archived": true}, wantBody: "conversation not found\n"},
		{name: "rename POST", method: http.MethodPost, sub: "rename",
			body: map[string]string{"title": "x"}, wantBody: "conversation not found\n"},
		{name: "model POST (clear)", method: http.MethodPost, sub: "model",
			body: map[string]string{"model": ""}, wantBody: "conversation not found\n"},
		{name: "approval-timeout POST", method: http.MethodPost, sub: "approval-timeout",
			body: map[string]any{"approval_timeout_seconds": 60}, wantBody: "conversation not found\n"},
		{name: "thinking_config PUT", method: http.MethodPut, sub: "thinking_config",
			body: map[string]any{"enabled": true, "budget_tokens": 0}, wantBody: "conversation not found\n"},
		{name: "thinking_config DELETE", method: http.MethodDelete, sub: "thinking_config", wantBody: "conversation not found\n"},
		{name: "truncate POST", method: http.MethodPost, sub: "truncate",
			body: map[string]any{"after_message_id": 1}, wantBody: "conversation not found\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/conversations/" + missing
			if tc.sub != "" {
				path += "/" + tc.sub
			}
			w := do(t, h, tc.method, path, tc.body, user)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s %s: got %d want 404 body=%q", tc.method, path, w.Code, w.Body.String())
			}
			if w.Body.String() != tc.wantBody {
				t.Fatalf("%s %s: 404 body %q, want %q", tc.method, path, w.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestConversationSubrouteUnmatched405 pins the dispatcher's default: a
// (sub, method) pair with no table entry answers exactly the 405 the old
// switch's default case produced — for both an unknown sub and a known sub
// probed with the wrong method.
func TestConversationSubrouteUnmatched405(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const user = "router@x.com"
	id := newRouteProbeConversation(t, h)

	cases := []struct {
		name   string
		method string
		sub    string
	}{
		{name: "known sub wrong method", method: http.MethodPatch, sub: "pin"},
		{name: "unknown sub", method: http.MethodGet, sub: "no-such-subroute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, tc.method, "/conversations/"+id+"/"+tc.sub, nil, user)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s /%s: got %d want 405 body=%q", tc.method, tc.sub, w.Code, w.Body.String())
			}
			if w.Body.String() != "method not allowed\n" {
				t.Fatalf("%s /%s: 405 body %q, want %q", tc.method, tc.sub, w.Body.String(), "method not allowed\n")
			}
		})
	}
}

// TestConversationRenameCapsAtRuneBoundary pins that a long title is capped
// at a UTF-8 rune boundary. The old title[:200] byte slice could cut a
// multi-byte character in half; Postgres rejects the invalid byte sequence
// and the user got a 500 for renaming a chat to a long non-ASCII title.
func TestConversationRenameCapsAtRuneBoundary(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()
	const user = "router@x.com"
	id := newRouteProbeConversation(t, h)

	// 3-byte runes: 200 is not a multiple of 3, so a byte slice at 200 lands
	// mid-rune. 70 of them is 210 bytes, past the cap.
	long := strings.Repeat("語", 70)
	w := do(t, h, http.MethodPost, "/conversations/"+id+"/rename", map[string]string{"title": long}, user)
	if w.Code != http.StatusOK {
		t.Fatalf("rename: got %d want 200 body=%q", w.Code, w.Body.String())
	}
	conv, err := s.store.Get(context.Background(), user, id)
	if err != nil || conv == nil {
		t.Fatalf("get: conv=%v err=%v", conv, err)
	}
	if !utf8.ValidString(conv.Title) {
		t.Fatalf("stored title is not valid UTF-8: %q", conv.Title)
	}
	if len(conv.Title) > 200 || len(conv.Title) != 198 {
		t.Fatalf("stored title is %d bytes, want 198 (66 whole runes)", len(conv.Title))
	}
}

// TestOptionalBodyMalformedIs400 pins that the optional-body routes refuse a
// present-but-malformed body instead of decoding it to the zero value and
// acting on that: share (would mint a non-expiring link), share-with-team
// (would un-share), branch (would report "must be a positive message id"),
// summarize (would silently ignore the model override).
func TestOptionalBodyMalformedIs400(t *testing.T) {
	s := serverFixture(t)
	s.agent = &fakeEngine{}
	h := s.Routes()
	const user = "router@x.com"
	id := newRouteProbeConversation(t, h)

	for _, sub := range []string{"share", "share-with-team", "branch", "summarize"} {
		t.Run(sub, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/conversations/"+id+"/"+sub, strings.NewReader(`{"expires_at": "not a number", "visible": "yes", "branch_point_message_id": "1"`))
			req.Header.Set("X-Chat-Server-Token", "tok")
			req.Header.Set("X-User-Email", user)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s with malformed body: got %d want 400 body=%q", sub, w.Code, w.Body.String())
			}
			if !strings.HasPrefix(w.Body.String(), "bad json") {
				t.Fatalf("%s: body %q, want a bad json 400", sub, w.Body.String())
			}
		})
	}
	// And the empty body still means "defaults": share mints a link.
	w := do(t, h, http.MethodPost, "/conversations/"+id+"/share", nil, user)
	if w.Code != http.StatusCreated {
		t.Fatalf("share with empty body: got %d want 201 body=%q", w.Code, w.Body.String())
	}
}
