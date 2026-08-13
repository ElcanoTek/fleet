package handlers

import (
	"context"
	"errors"
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

const (
	liveEpoch  = "1122334455667788"
	staleEpoch = "0000000000000000"
)

// epochHandler is newHeaderTrustHandler plus a chat-plane epoch seam. A nil
// lookup (the seam unwired) is the caller's way of asking for the pre-seam
// behavior.
func epochHandler(lookup ChatSessionEpochProvider) *Handlers {
	h := newHeaderTrustHandler()
	h.SetChatSessionEpochProvider(lookup)
	return h
}

func liveEpochLookup(context.Context, string) (string, error) { return liveEpoch, nil }

// epochRequest is a header-trust request carrying an epoch claim; an empty
// epoch sends no claim at all (the elcano_auth / moc-bearer case).
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
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	do := func(h *Handlers, epoch string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.AdminOrUserAuthMiddleware(ok).ServeHTTP(rr, epochRequest(http.MethodGet, "/api/me", epoch))
		return rr
	}

	t.Run("a claim matching the chat plane is admitted", func(t *testing.T) {
		rr := do(epochHandler(liveEpochLookup), liveEpoch)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d want 200 (body %q)", rr.Code, rr.Body.String())
		}
	})

	// The defect: an admin resets the compromised account's password and the
	// stolen cookie keeps working here.
	t.Run("a stale claim is refused with the revocation verdict", func(t *testing.T) {
		rr := do(epochHandler(liveEpochLookup), staleEpoch)
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

	// An unreachable chat store says nothing about the session. Answering the
	// revoked verdict would delete a valid cookie and sign the whole Operations
	// Center out over a database blip, so this is the same retryable 500 the
	// membership lookup already returns on error.
	t.Run("a failed lookup is a retryable 500, not a revocation", func(t *testing.T) {
		down := func(context.Context, string) (string, error) { return "", errors.New("chat store unreachable") }
		rr := do(epochHandler(down), liveEpoch)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status %d want 500 (body %q)", rr.Code, rr.Body.String())
		}
		if rr.Header().Get(headerSessionRevoked) != "" {
			t.Error("a lookup failure must not carry the revocation verdict")
		}
	})

	// The magic-link (elcano_auth) cookie is minted by the auth service, which
	// cannot add a claim; a moc bearer carries none either. Both stay admitted.
	t.Run("no claim is admitted", func(t *testing.T) {
		rr := do(epochHandler(liveEpochLookup), "")
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d want 200 (body %q)", rr.Code, rr.Body.String())
		}
	})

	// The seam is what carries the epoch across the two databases (ADR-0005);
	// unwired, there is no chat plane in the process to have revoked anything.
	t.Run("an unwired seam ignores the claim", func(t *testing.T) {
		rr := do(epochHandler(nil), staleEpoch)
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d want 200 (body %q)", rr.Code, rr.Body.String())
		}
	})

	// Membership outranks the session: a deleted account must still get the 403
	// the UI keys on, not a "log in again" verdict it cannot act on.
	t.Run("a non-member is still 403, without a lookup", func(t *testing.T) {
		called := false
		h := epochHandler(func(context.Context, string) (string, error) {
			called = true
			return liveEpoch, nil
		})
		req := epochRequest(http.MethodGet, "/api/me", staleEpoch)
		req.Header.Set("X-User-Email", "stranger@example.com")
		rr := httptest.NewRecorder()
		h.AdminOrUserAuthMiddleware(ok).ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status %d want 403 (body %q)", rr.Code, rr.Body.String())
		}
		if called {
			t.Error("epoch lookup ran for a non-member")
		}
	})
}

// POST /tasks and POST /upload live OUTSIDE AdminOrUserAuthMiddleware. They
// share headerTrustUser precisely so a gate cannot hold on one and not the
// others — task create/rerun and the upload staging area are the highest-value
// routes on this plane.
func TestHeaderTrust_SessionEpochOnRoutesOutsideTheAuthMiddleware(t *testing.T) {
	t.Run("authorizeTaskCreator", func(t *testing.T) {
		h := epochHandler(liveEpochLookup)
		rr := httptest.NewRecorder()
		if _, ok := h.authorizeTaskCreator(rr, epochRequest(http.MethodPost, "/tasks", staleEpoch)); ok {
			t.Fatal("a stale session was authorized to create a task")
		}
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status %d want 401 (body %q)", rr.Code, rr.Body.String())
		}
	})

	t.Run("HandleUpload", func(t *testing.T) {
		h := epochHandler(liveEpochLookup)
		rr := httptest.NewRecorder()
		h.HandleUpload(rr, epochRequest(http.MethodPost, "/upload", staleEpoch))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status %d want 401 (body %q)", rr.Code, rr.Body.String())
		}
	})
}
