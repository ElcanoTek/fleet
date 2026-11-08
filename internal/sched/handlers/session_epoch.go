// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Session-epoch revocation for the web-tier header-trust path (#157). The
// Operations Center and chat are reached with the SAME elcano_session cookie, so
// a password reset that evicted the cookie from chat alone would leave the
// attacker every /api/orchestrator/* route — datasets, task create/rerun, logs,
// task workspace files, admin budgets — for the rest of the cookie's 14 days.
// The web tier forwards the cookie's epoch claim next to X-User-Email, and
// headerTrustUser checks it here.
//
// The epoch is derived from the account's ONE row in core.users (ADR-0041,
// identity.SessionEpochSQL) — the same row and the same derivation the chat
// membership check reads. Since the one identity table (#2049 M4, ADR-0105)
// this plane reads it directly; there is no lookup seam to wire, so no wiring
// can forget it.

import (
	"crypto/subtle"
	"net/http"
)

// headerSessionEpoch is the claim the web tier read out of the session
// cookie it verified; headerSessionRevoked marks the verdict below for the
// web tier funnel, which must read it without touching a body it streams
// straight through. Both mirror internal/httpapi's spelling — the two backends
// are separate packages, but the wire contract with the one proxy is shared.
const (
	headerSessionEpoch    = "X-User-Session-Epoch"
	headerSessionRevoked  = "X-Session-Revoked"
	headerSessionSource   = "X-User-Session-Source"
	headerExternalIssuer  = "X-External-Issuer"
	headerExternalSubject = "X-External-Subject"
)

// checkSessionEpoch gates a header-trust request on its session-epoch claim.
// Returns false when it has written the response.
//
// A request with NO claim is admitted: the Ed25519 elcano_auth cookie is minted
// by the auth service (revocable there) and a moc bearer is its own credential,
// so neither carries an epoch. This mirrors the chat plane's rule exactly —
// the chat-minted cookies this defends are refused by the web tier outright
// when the claim is missing (web/src/app/lib/auth.ts#verifySessionToken).
//
// A lookup FAILURE is a 500, not a revocation: an unreachable or slow database
// says nothing about the session, and answering the revoked verdict would delete
// a valid cookie and sign the whole Operations Center out over a database blip.
// A 500 leaves the session intact and the request retryable — the same posture
// headerTrustUser already takes when the membership lookup itself errors.
func (h *Handlers) checkSessionEpoch(w http.ResponseWriter, r *http.Request, email string) bool {
	claim := r.Header.Get(headerSessionEpoch)
	if claim == "" {
		return true
	}
	var live string
	var err error
	ids := h.storage.Identity()
	if r.Header.Get(headerSessionSource) == "oidc" {
		issuer := r.Header.Get(headerExternalIssuer)
		subject := r.Header.Get(headerExternalSubject)
		if issuer == "" || subject == "" {
			w.Header().Set(headerSessionRevoked, "1")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session_revoked"})
			return false
		}
		live, err = ids.LookupExternalSessionEpoch(r.Context(), issuer, subject)
	} else {
		live, err = ids.SessionEpoch(r.Context(), email)
	}
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
