package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
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
