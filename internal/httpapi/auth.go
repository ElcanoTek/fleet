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
// Both are mandatory on every non-/healthz endpoint. A third header,
// X-User-Session-Epoch, is optional — see headerSessionEpoch.
package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// headerSessionEpoch carries the session-epoch claim the Next.js tier read out
// of the session cookie it verified. membershipMiddleware compares it against
// the account's live epoch, which is what makes a password change evict the
// sessions minted before it: the session token itself is a stateless HMAC the
// Next tier verifies locally, so this lookup is the only place a revocation
// decision can be made.
//
// It is trusted exactly as far as X-User-Email is, and no further: both arrive
// over the X-Chat-Server-Token channel, and anything holding that secret can
// already assert an arbitrary identity outright, so forwarding the claim opens
// no new spoofing surface.
//
// A request carrying NO claim is admitted. The Ed25519 elcano_auth cookie is
// minted by the auth service, which chat cannot add a claim to, so those
// sessions have no epoch and stay revocable only there; the chat-minted cookies
// that this defends are refused by the Next tier outright when the claim is
// missing (web/src/app/lib/auth.ts#verifySessionToken).
const headerSessionEpoch = "X-User-Session-Epoch"

type ctxKey string

const (
	ctxKeyUser ctxKey = "user_email"
	// ctxKeyRole carries the caller's RBAC role (#237), enriched by
	// membershipMiddleware from the users table so downstream gates
	// (rejectViewerWrites, adminMiddleware) don't each re-query. Absent (empty)
	// on the test seam path, which treats every member as a plain member.
	ctxKeyRole ctxKey = "user_role"
)

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
// for deployment-wide, non-secret, non-user-scoped data the pre-auth UI needs:
// /theme.css (the brand palette that themes the login page before a session
// exists), /brand/logo (the mark that page may render), /brand/share-image
// (the og:image anonymous unfurl scrapers fetch), /brand/meta (the app
// name, login copy, and share strings the shell and unfurl scrapers need), and
// /shared/ (whose authorization is the share token in the path). Only the
// trusted Next.js layer holds the token, so the browser still cannot reach
// chat-server directly; dropping the X-User-Email requirement is what lets the
// un-authenticated login page request them.
//
// The bar for adding a route here is that its response is public BY
// CONSTRUCTION — already visible to anyone who can load the login page or scrape
// a shared link — not merely non-sensitive-looking. /client-config stays
// member-gated because it also carries workspace content.
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

// roleFromCtx returns the caller's RBAC role enriched by membershipMiddleware,
// or "" when unknown (the test seam path). "" is treated as a plain member by
// the gates — never as admin, so an un-enriched request can't escalate.
func roleFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRole).(string)
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
		ctx := r.Context()

		// Test seam: when isMember is injected, do a membership-only check and
		// skip role/team enrichment (the fake store embeds a nil *store.Store,
		// so GetUser would panic). Production leaves isMember nil and takes the
		// enriching path below.
		if s.isMember != nil {
			ok, err := s.isMember(ctx, email)
			if err != nil {
				http.Error(w, "membership check failed", http.StatusInternalServerError)
				return
			}
			if !ok {
				writeNotAMember(w)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Production: a single lookup both admits the user AND enriches the
		// request with their role + team (#237), so downstream gates don't
		// re-query. ErrUserNotFound is the "valid cookie, not a chat user" case.
		u, err := s.store.GetUser(ctx, email)
		if errors.Is(err, store.ErrUserNotFound) {
			writeNotAMember(w)
			return
		}
		if err != nil {
			http.Error(w, "membership check failed", http.StatusInternalServerError)
			return
		}
		if claim := r.Header.Get(headerSessionEpoch); claim != "" &&
			subtle.ConstantTimeCompare([]byte(claim), []byte(u.SessionEpoch)) != 1 {
			writeSessionRevoked(w)
			return
		}
		ctx = context.WithValue(ctx, ctxKeyRole, u.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeNotAMember emits the distinct, machine-readable 403 the Next.js layer
// keys on to render the no-access page instead of bouncing back to login.
func writeNotAMember(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"not_a_member"}`))
}

// headerSessionRevoked marks the response below for the Next.js proxy funnel.
// The verdict has to be readable without touching the body, because that funnel
// streams the body straight through to the browser (chatServerPassthrough, the
// SSE routes) and consuming it there would break those callers.
const headerSessionRevoked = "X-Session-Revoked"

// writeSessionRevoked answers a request whose session-epoch claim no longer
// matches the account. 401 rather than the membership 403: the account is fine,
// the session is not, and re-authenticating is the fix — the web client's
// classifier already routes 401 to the login page, and the proxy funnel drops
// the stale cookie on the way out so /login does not bounce it straight back.
func writeSessionRevoked(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerSessionRevoked, "1")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"session_revoked"}`))
}

type sessionEpochResponse struct {
	SessionEpoch string `json:"session_epoch"`
}

// handleSessionEpoch serves GET /auth/session-epoch — the epoch the Next.js tier
// stamps into a session cookie it is about to mint.
//
// It sits on authMiddleware alone, deliberately outside the membership gate:
// both mint paths run before any session exists, and an email chat has not
// provisioned must still get a well-formed answer rather than a 403 the login
// flow would have to special-case. Unlike /auth/verify this is not reachable
// from the login form, so it is not an enumeration surface: the only caller is
// the shared-token-holding Next.js tier, which can already assert any identity
// it likes.
func (s *Server) handleSessionEpoch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	epoch, err := s.store.SessionEpoch(r.Context(), userFromCtx(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionEpochResponse{SessionEpoch: epoch})
}

// rejectViewerWrites blocks the read-only "viewer" role (#237) from MUTATING a
// route it wraps, returning 403 {"error":"read_only"}. It is method-aware:
// safe methods (GET/HEAD/OPTIONS) always pass so a viewer keeps full read
// access, and only state-changing methods (POST/PATCH/PUT/DELETE) are gated.
// This lets it wrap the mixed read+write handlers (/conversations, …) without a
// per-method split at every call site. It runs AFTER membershipMiddleware so
// the role is in context; an un-enriched request (role "", the test seam) is
// treated as a non-viewer and passes — viewer is the only role this stops.
func (s *Server) rejectViewerWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if roleFromCtx(r.Context()) == store.RoleViewer {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"read_only"}`))
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
