package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The Next-proxy header-trust path (#157) is what lets a /chat-cookie user reach
// the Operations Center without a second login: the Next layer forwards
// X-User-Email guarded by the shared X-Orchestrator-Server-Token. It must mirror
// chat-server: token-first (fail-closed, no fall-through), then a non-empty
// email, then the membership gate.
func newHeaderTrustHandler() *Handlers {
	member := &models.User{ID: uuid.New(), Username: "alice@elcanotek.com", Role: "client"}
	return &Handlers{
		// AdminAPIKey non-empty so verifyAdminKey doesn't fail-open on the
		// empty/empty hash match (matches the elcano cookie test's setup).
		config: Config{AdminAPIKey: "admin-secret", SharedToken: "topsecret"},
		memberLookup: func(_ context.Context, email string) (*models.User, error) {
			if email == "alice@elcanotek.com" {
				return member, nil
			}
			return nil, sql.ErrNoRows
		},
	}
}

func TestAdminOrUserAuthMiddleware_HeaderTrust(t *testing.T) {
	h := newHeaderTrustHandler()
	var seenUser string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUser = ""
		if u := GetUserFromContext(r.Context()); u != nil {
			seenUser = u.Username
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := h.AdminOrUserAuthMiddleware(final)

	do := func(token, email string) *httptest.ResponseRecorder {
		seenUser = ""
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		if token != "" {
			req.Header.Set("X-Orchestrator-Server-Token", token)
		}
		if email != "" {
			req.Header.Set("X-User-Email", email)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	t.Run("valid token + member admits and injects user", func(t *testing.T) {
		rr := do("topsecret", "alice@elcanotek.com")
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d want 200", rr.Code)
		}
		if seenUser != "alice@elcanotek.com" {
			t.Errorf("user %q not injected into context", seenUser)
		}
	})

	t.Run("email is normalized (case/space)", func(t *testing.T) {
		rr := do("topsecret", "  Alice@Elcanotek.com  ")
		if rr.Code != http.StatusOK {
			t.Errorf("status %d want 200 (email should be lowercased/trimmed)", rr.Code)
		}
	})

	t.Run("wrong token is rejected with NO fall-through", func(t *testing.T) {
		rr := do("wrong", "alice@elcanotek.com")
		if rr.Code != http.StatusForbidden {
			t.Errorf("status %d want 403 (present-but-wrong token must fail closed)", rr.Code)
		}
	})

	t.Run("valid token but missing email is a 400", func(t *testing.T) {
		rr := do("topsecret", "")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status %d want 400", rr.Code)
		}
	})

	t.Run("valid token but non-member is 403", func(t *testing.T) {
		rr := do("topsecret", "stranger@example.com")
		if rr.Code != http.StatusForbidden {
			t.Errorf("status %d want 403 (not_a_member)", rr.Code)
		}
	})

	t.Run("absent token falls through to other paths (here: 401, no creds)", func(t *testing.T) {
		rr := do("", "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status %d want 401 (no credential at all)", rr.Code)
		}
	})
}

// POST /tasks (and /tasks/batch, /tasks/estimate) live OUTSIDE the
// AdminOrUserAuthMiddleware group — their auth is authorizeTaskCreator. It must
// honor the same header-trust path with the same fail-closed semantics (both
// now share headerTrustUser), or the web UI's elcano-cookie login path clears
// the CSRF gate only to die with 401 here.
func TestAuthorizeTaskCreator_HeaderTrust(t *testing.T) {
	h := newHeaderTrustHandler()
	do := func(token, email string) (*httptest.ResponseRecorder, taskCreator, bool) {
		req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
		if token != "" {
			req.Header.Set("X-Orchestrator-Server-Token", token)
		}
		if email != "" {
			req.Header.Set("X-User-Email", email)
		}
		rr := httptest.NewRecorder()
		creator, ok := h.authorizeTaskCreator(rr, req)
		return rr, creator, ok
	}

	t.Run("valid token + member authorizes with the member as creator", func(t *testing.T) {
		rr, creator, ok := do("topsecret", "alice@elcanotek.com")
		if !ok {
			t.Fatalf("want authorized, got %d %q", rr.Code, rr.Body.String())
		}
		if creator.creatorID == nil || creator.creatorUsername != "alice@elcanotek.com" {
			t.Errorf("creator not resolved from header trust: %+v", creator)
		}
		if creator.hasAdminPermission {
			t.Errorf("header-trust client-role member must not carry admin permission")
		}
	})

	t.Run("wrong token fails closed with 403, no fall-through", func(t *testing.T) {
		rr, _, ok := do("wrong", "alice@elcanotek.com")
		if ok || rr.Code != http.StatusForbidden {
			t.Errorf("ok=%v status=%d, want rejected 403 (present-but-wrong token must fail closed)", ok, rr.Code)
		}
	})

	t.Run("valid token + non-member is 403", func(t *testing.T) {
		rr, _, ok := do("topsecret", "stranger@example.com")
		if ok || rr.Code != http.StatusForbidden {
			t.Errorf("ok=%v status=%d, want 403 (not_a_member)", ok, rr.Code)
		}
	})

	t.Run("valid token, missing email is 400", func(t *testing.T) {
		rr, _, ok := do("topsecret", "")
		if ok || rr.Code != http.StatusBadRequest {
			t.Errorf("ok=%v status=%d, want 400", ok, rr.Code)
		}
	})

	t.Run("absent token still falls through to other paths (401 with no creds)", func(t *testing.T) {
		rr, _, ok := do("", "")
		if ok || rr.Code != http.StatusUnauthorized {
			t.Errorf("ok=%v status=%d, want 401", ok, rr.Code)
		}
	})
}

// /upload also lives outside the auth group and stages files consumed only by
// a later task create, so it honors the same header-trust path.
func TestHandleUpload_HeaderTrust(t *testing.T) {
	h := newHeaderTrustHandler()
	do := func(token, email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/upload", nil)
		if token != "" {
			req.Header.Set("X-Orchestrator-Server-Token", token)
		}
		if email != "" {
			req.Header.Set("X-User-Email", email)
		}
		rr := httptest.NewRecorder()
		h.HandleUpload(rr, req)
		return rr
	}

	t.Run("wrong token fails closed with 403", func(t *testing.T) {
		rr := do("wrong", "alice@elcanotek.com")
		if rr.Code != http.StatusForbidden {
			t.Errorf("status %d want 403", rr.Code)
		}
	})

	t.Run("valid token + member clears auth (fails later on the missing multipart body)", func(t *testing.T) {
		rr := do("topsecret", "alice@elcanotek.com")
		// 400 = ParseMultipartForm rejected the empty body AFTER auth passed;
		// 401/403 here would mean the header-trust path was not honored.
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status %d want 400 (past auth, no multipart body)", rr.Code)
		}
	})

	t.Run("valid token + non-member is 403", func(t *testing.T) {
		rr := do("topsecret", "stranger@example.com")
		if rr.Code != http.StatusForbidden {
			t.Errorf("status %d want 403 (not_a_member)", rr.Code)
		}
	})
}

// POST /tasks/estimate is creation-shaped (same body, same rate limiter as
// POST /tasks) and sits outside the auth middleware group like its siblings.
// It used to enforce a hand-rolled auth copy that predated header trust, so
// the pre-submission cost forecast was silently dead ("Estimate failed:
// Unauthorized") for every cookie-path Operations Center user — fail-closed,
// but broken. It now shares authorizeTaskCreator, including the session-epoch
// revocation gate.
func TestEstimateTask_HeaderTrust(t *testing.T) {
	newEstimateHandler := func() *Handlers {
		h := newHeaderTrustHandler()
		h.config.DefaultTaskModel = "anthropic/claude-haiku-4-5"
		h.config.DefaultMaxIterations = 5
		return h
	}
	do := func(h *Handlers, token, email, epoch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/tasks/estimate",
			strings.NewReader(`{"prompt":"estimate this prompt please"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Orchestrator-Server-Token", token)
		}
		if email != "" {
			req.Header.Set("X-User-Email", email)
		}
		if epoch != "" {
			req.Header.Set("X-User-Session-Epoch", epoch)
		}
		rr := httptest.NewRecorder()
		h.EstimateTask(rr, req)
		return rr
	}

	t.Run("valid token + member gets a forecast", func(t *testing.T) {
		rr := do(newEstimateHandler(), "topsecret", "alice@elcanotek.com", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d want 200 (header-trust user must be able to estimate): %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("wrong token fails closed with 403", func(t *testing.T) {
		rr := do(newEstimateHandler(), "wrong", "alice@elcanotek.com", "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("status %d want 403", rr.Code)
		}
	})

	t.Run("valid token + non-member is 403", func(t *testing.T) {
		rr := do(newEstimateHandler(), "topsecret", "stranger@example.com", "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("status %d want 403 (not_a_member)", rr.Code)
		}
	})

	t.Run("revoked session epoch is 401", func(t *testing.T) {
		h := newEstimateHandler()
		h.chatSessionEpoch = func(_ context.Context, _ string) (string, error) { return "live-epoch", nil }
		rr := do(h, "topsecret", "alice@elcanotek.com", "stale-epoch")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status %d want 401 (session-epoch revocation must apply to estimate too)", rr.Code)
		}
		if rr.Header().Get("X-Session-Revoked") != "1" {
			t.Error("missing X-Session-Revoked verdict header")
		}
	})

	t.Run("matching session epoch is admitted", func(t *testing.T) {
		h := newEstimateHandler()
		h.chatSessionEpoch = func(_ context.Context, _ string) (string, error) { return "live-epoch", nil }
		rr := do(h, "topsecret", "alice@elcanotek.com", "live-epoch")
		if rr.Code != http.StatusOK {
			t.Errorf("status %d want 200: %s", rr.Code, rr.Body.String())
		}
	})
}
