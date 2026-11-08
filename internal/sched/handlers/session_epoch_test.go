package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Operations Center authenticates with the SAME elcano_session cookie chat
// does, so a password reset that only evicted the cookie from chat left the
// attacker every /api/orchestrator/* route for the rest of its 14 days. These
// pin the header-trust path's half of the revocation: the forwarded claim is
// compared against the chat plane's live epoch, and only a genuine MISMATCH
// ends the session.
//
// All three headerTrustUser callers are covered, because the check has to hold
// for the routes outside the auth middleware too (task create, upload).

// Since the one identity table (#2049 M4) the live epoch is the account's own
// row in core.users — the same row and the same derivation chat reads — so
// these run against the database: alice's live epoch is whatever her row
// derives to, and a password write moves it.
const staleEpoch = "0000000000000000"

// epochHandler is newHeaderTrustHandler, returning alice's live epoch.
func epochHandler(t *testing.T) (*Handlers, string) {
	t.Helper()
	h := newHeaderTrustHandler(t)
	live, err := h.storage.Identity().SessionEpoch(t.Context(), "alice@elcanotek.com")
	if err != nil {
		t.Fatalf("live epoch: %v", err)
	}
	return h, live
}

// epochRequest is a header-trust request carrying an epoch claim; an empty
// epoch sends no claim at all (the elcano_auth case).
func epochRequest(method, path, epoch string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Orchestrator-Server-Token", "topsecret")
	req.Header.Set("X-User-Email", "alice@elcanotek.com")
	if epoch != "" {
		req.Header.Set(headerSessionEpoch, epoch)
	}
	return req
}

func TestHeaderTrust_SessionEpoch(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	do := func(h *Handlers, epoch string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.AdminOrUserAuthMiddleware(ok).ServeHTTP(rr, epochRequest(http.MethodGet, "/api/me", epoch))
		return rr
	}

	t.Run("a claim matching the account's row is admitted", func(t *testing.T) {
		h, live := epochHandler(t)
		rr := do(h, live)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d want 200 (body %q)", rr.Code, rr.Body.String())
		}
	})

	// The defect: an admin resets the compromised account's password and the
	// stolen cookie keeps working here.
	t.Run("a stale claim is refused with the revocation verdict", func(t *testing.T) {
		h, _ := epochHandler(t)
		rr := do(h, staleEpoch)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status %d want 401 (body %q)", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get(headerSessionRevoked); got != "1" {
			t.Errorf("%s = %q, want \"1\" — the proxy funnel keys on it to drop the stale cookie",
				headerSessionRevoked, got)
		}
		if body := rr.Body.String(); !strings.Contains(body, "session_revoked") {
			t.Errorf("body = %q, want it to mark session_revoked", body)
		}
	})

	// A password write (a reset) moves the epoch: the cookie minted before it
	// stops working here at once, with no seam to wire (ADR-0041).
	t.Run("a password write revokes the earlier claim", func(t *testing.T) {
		h, live := epochHandler(t)
		if err := h.storage.Identity().SetPassword(t.Context(), "alice@elcanotek.com", "placeholder-not-a-secret"); err != nil {
			t.Fatal(err)
		}
		if rr := do(h, live); rr.Code != http.StatusUnauthorized {
			t.Fatalf("status %d want 401 after a password write (body %q)", rr.Code, rr.Body.String())
		}
	})

	// An unreachable database says nothing about the session. Answering the
	// revoked verdict would delete a valid cookie and sign the whole Operations
	// Center out over a database blip, so this is a retryable 500.
	t.Run("a failed lookup is a retryable 500, not a revocation", func(t *testing.T) {
		h, live := epochHandler(t)
		_ = h.storage.DB().Conn().Close()
		rr := do(h, live)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status %d want 500 (body %q)", rr.Code, rr.Body.String())
		}
		if rr.Header().Get(headerSessionRevoked) != "" {
			t.Error("a lookup failure must not carry the revocation verdict")
		}
	})

	// The magic-link (elcano_auth) cookie is minted by the auth service, which
	// cannot add a claim. It stays admitted.
	t.Run("no claim is admitted", func(t *testing.T) {
		h, _ := epochHandler(t)
		rr := do(h, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d want 200 (body %q)", rr.Code, rr.Body.String())
		}
	})

	// Membership outranks the session: a deleted account must still get the 403
	// the UI keys on, not a "log in again" verdict it cannot act on.
	t.Run("a non-member is still 403", func(t *testing.T) {
		h, _ := epochHandler(t)
		req := epochRequest(http.MethodGet, "/api/me", staleEpoch)
		req.Header.Set("X-User-Email", "stranger@example.com")
		rr := httptest.NewRecorder()
		h.AdminOrUserAuthMiddleware(ok).ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status %d want 403 (body %q)", rr.Code, rr.Body.String())
		}
	})
}

func TestHeaderTrust_ExternalSessionEpoch(t *testing.T) {
	t.Parallel()

	h, _ := epochHandler(t)
	if _, err := h.storage.DB().Conn().ExecContext(t.Context(), `INSERT INTO core.external_auth_epochs(issuer, subject, email, epoch, created_at, updated_at)
		VALUES('https://auth.example.com', 'account-123', 'alice@elcanotek.com', 'external-live', 0, 0)
		ON CONFLICT (issuer, subject) DO UPDATE SET epoch = EXCLUDED.epoch`); err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	req := epochRequest(http.MethodGet, "/api/me", "external-live")
	req.Header.Set("X-User-Session-Source", "oidc")
	req.Header.Set("X-External-Issuer", "https://auth.example.com")
	req.Header.Set("X-External-Subject", "account-123")
	rr := httptest.NewRecorder()
	h.AdminOrUserAuthMiddleware(ok).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fresh external session status = %d body %q", rr.Code, rr.Body.String())
	}

	req = epochRequest(http.MethodGet, "/api/me", "stale-external")
	req.Header.Set("X-User-Session-Source", "oidc")
	req.Header.Set("X-External-Issuer", "https://auth.example.com")
	req.Header.Set("X-External-Subject", "account-123")
	rr = httptest.NewRecorder()
	h.AdminOrUserAuthMiddleware(ok).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("stale external session status = %d body %q", rr.Code, rr.Body.String())
	}
}

// POST /tasks and POST /upload live OUTSIDE AdminOrUserAuthMiddleware. They
// share headerTrustUser precisely so a gate cannot hold on one and not the
// others — task create/rerun and the upload staging area are the highest-value
// routes on this plane.
func TestHeaderTrust_SessionEpochOnRoutesOutsideTheAuthMiddleware(t *testing.T) {
	t.Parallel()

	t.Run("authorizeTaskCreator", func(t *testing.T) {
		h, _ := epochHandler(t)
		rr := httptest.NewRecorder()
		if _, ok := h.authorizeTaskCreator(rr, epochRequest(http.MethodPost, "/tasks", staleEpoch)); ok {
			t.Fatal("a stale session was authorized to create a task")
		}
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status %d want 401 (body %q)", rr.Code, rr.Body.String())
		}
	})

	t.Run("HandleUpload", func(t *testing.T) {
		h, _ := epochHandler(t)
		rr := httptest.NewRecorder()
		h.HandleUpload(rr, epochRequest(http.MethodPost, "/upload", staleEpoch))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status %d want 401 (body %q)", rr.Code, rr.Body.String())
		}
	})
}
