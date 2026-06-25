// Package httpapi holds the HTTP handlers for chat-server.
//
// Auth model: chat-server is NOT exposed to browsers directly. The Next.js
// API routes verify the user's session cookie, then forward to chat-server
// with two headers:
//   - X-Chat-Server-Token: a shared secret proving the request came from the
//     trusted Next.js layer (not from an attacker who found the port open).
//   - X-User-Email: the authenticated user's email, used for row-level
//     scoping of every SQL query.
//
// Both are mandatory on every non-/healthz endpoint.
package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

type ctxKey string

const ctxKeyUser ctxKey = "user_email"

// authMiddleware enforces the shared-secret + user-email headers.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Chat-Server-Token")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.sharedToken)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Normalize once at the chokepoint, matching the store's
		// normalizeEmail. Conversation scoping, rate-limit buckets and
		// approvals all key off this value verbatim — without this,
		// Brad@x.com and brad@x.com both pass membership (the store
		// normalizes for that check) but get disjoint conversation
		// namespaces and separate rate-limit buckets.
		user := strings.ToLower(strings.TrimSpace(r.Header.Get("X-User-Email")))
		if user == "" {
			http.Error(w, "missing X-User-Email", http.StatusBadRequest)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUser, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// tokenOnlyMiddleware enforces the shared-secret but NOT a user identity. It is
// for deployment-wide, non-secret, non-user-scoped data the pre-auth UI needs —
// today only /theme.css, the brand palette that themes the login page before a
// session exists. Only the trusted Next.js layer holds the token, so the
// browser still cannot reach chat-server directly; dropping the X-User-Email
// requirement is what lets the un-authenticated login page request it.
func (s *Server) tokenOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Chat-Server-Token")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.sharedToken)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// userFromCtx pulls the authenticated email out of the request context.
func userFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyUser).(string)
	return v
}

// membershipMiddleware enforces the scoped-tier user-list gate. A request
// that already cleared authMiddleware (valid shared token + X-User-Email)
// is admitted only if that email belongs to a provisioned chat user. This
// is what lets people authenticate via the shared elcano_auth cookie minted
// by the auth service while chat keeps owning WHO may use chat — the cookie
// says who you are, this gate says whether you're allowed in.
//
// It is deliberately NOT folded into authMiddleware: /auth/verify (the
// password pre-login check) must stay reachable for not-yet-known emails,
// otherwise a 403 here would let the response be used to enumerate the
// user-list. Wrap user-data routes with this; leave /auth/verify on auth
// alone.
func (s *Server) membershipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := userFromCtx(r.Context())
		check := s.isMember
		if check == nil {
			check = s.store.IsUser
		}
		ok, err := check(r.Context(), email)
		if err != nil {
			http.Error(w, "membership check failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			// Distinct, machine-readable body so the Next.js layer can tell
			// "valid cookie but not a chat user" apart from other 403s and
			// render the no-access page instead of bouncing back to login.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"not_a_member"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleMembership is a no-op endpoint behind auth+membership. Reaching it
// (200) means X-User-Email belongs to a provisioned user; a non-member is
// rejected by membershipMiddleware with 403 {"error":"not_a_member"} before
// this runs. The Next.js entry check hits it to decide whether to show the
// app or the no-access page for elcano_auth sessions.
func (s *Server) handleMembership(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"member":true}`))
}
