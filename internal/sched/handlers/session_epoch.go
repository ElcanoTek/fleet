// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Session-epoch revocation for the Next-proxy header-trust path (#157). The
// Operations Center and chat are reached with the SAME elcano_session cookie, so
// a password reset that evicted the cookie from chat alone would leave the
// attacker every /api/orchestrator/* route — datasets, task create/rerun, logs,
// task workspace files, admin budgets — for the rest of the cookie's 14 days.
// The Next tier forwards the cookie's epoch claim next to X-User-Email, and
// headerTrustUser checks it here.
//
// The epoch lives in the CHAT store's users table. This plane cannot read that
// schema — chat and sched are separate databases (ADR-0005) — so the value
// arrives through a lookup seam cmd/fleet injects over the chat store, the same
// shape as the chat usage/adoption/account seams. It stays a per-request lookup
// keyed by email; nothing here joins the two schemas.

import (
	"context"
	"crypto/subtle"
	"net/http"
)

// headerSessionEpoch is the claim the Next.js tier read out of the session
// cookie it verified; headerSessionRevoked marks the verdict below for the
// Next.js proxy funnel, which must read it without touching a body it streams
// straight through. Both mirror internal/httpapi's spelling — the two backends
// are separate packages, but the wire contract with the one proxy is shared.
const (
	headerSessionEpoch   = "X-User-Session-Epoch"
	headerSessionRevoked = "X-Session-Revoked"
)

// ChatSessionEpochProvider resolves an email's CURRENT chat-plane session epoch
// — the value a forwarded claim must still match. Injected by cmd/fleet via
// SetChatSessionEpochProvider (store.Store.SessionEpoch); nil means this process
// has no chat plane to ask, and the claim is then ignored rather than guessed
// at. That is a wiring-time property, not a runtime one: `fleet serve` opens the
// chat store before it builds these handlers and always wires the seam, so the
// nil case belongs to tests and embedders that run the orchestrator alone —
// deployments where no elcano_session cookie exists to revoke.
type ChatSessionEpochProvider func(ctx context.Context, email string) (string, error)

// SetChatSessionEpochProvider wires the chat-plane epoch lookup (see
// ChatSessionEpochProvider). Call before serving traffic.
func (h *Handlers) SetChatSessionEpochProvider(fn ChatSessionEpochProvider) {
	h.chatSessionEpoch = fn
}

// checkSessionEpoch gates a header-trust request on its session-epoch claim.
// Returns false when it has written the response.
//
// A request with NO claim is admitted: the Ed25519 elcano_auth cookie is minted
// by the auth service (revocable there) and a moc bearer is its own credential,
// so neither carries an epoch. This mirrors the chat plane's rule exactly —
// the chat-minted cookies this defends are refused by the Next tier outright
// when the claim is missing (web/src/app/lib/auth.ts#verifySessionToken).
//
// A lookup FAILURE is a 500, not a revocation: an unreachable or slow chat store
// says nothing about the session, and answering the revoked verdict would delete
// a valid cookie and sign the whole Operations Center out over a database blip.
// A 500 leaves the session intact and the request retryable — the same posture
// headerTrustUser already takes when the membership lookup itself errors.
func (h *Handlers) checkSessionEpoch(w http.ResponseWriter, r *http.Request, email string) bool {
	claim := r.Header.Get(headerSessionEpoch)
	if claim == "" || h.chatSessionEpoch == nil {
		return true
	}
	live, err := h.chatSessionEpoch(r.Context(), email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Session check failed")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(claim), []byte(live)) != 1 {
		w.Header().Set(headerSessionRevoked, "1")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session_revoked"})
		return false
	}
	return true
}
