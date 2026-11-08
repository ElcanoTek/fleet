// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// The user principal on the orchestrator API (#2050, ADR-0108).
//
// The browser gateway (internal/webui) verifies every session — the
// elcano_session and elcano_auth cookies alike — and calls these handlers
// in-process with a authn.Principal in the request context; this server
// never reads a cookie or an identity header itself. The Operations Center
// and chat are reached with the SAME cookie, so the session-epoch check runs
// here too: a password reset that evicted the cookie from chat alone would
// otherwise leave every /api/orchestrator/* route — datasets, task
// create/rerun, logs, task workspace files, admin budgets — open for the rest
// of the cookie's 14 days. authn.Resolve is the one lookup both planes
// run: the account's single core.users row (#2049 M4) admits it and carries
// the live epoch.

import (
	"context"
	"errors"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/identity"
	authn "github.com/ElcanoTek/fleet/internal/principal"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// headerSessionRevoked marks the session-revoked verdict for the gateway's
// funnel, which must read it without touching a body it streams straight
// through. It mirrors internal/httpapi's spelling: the two backends are
// separate packages, but the contract with the one gateway is shared.
const headerSessionRevoked = "X-Session-Revoked"

// writeSessionRevoked answers a request whose session-epoch claim no longer
// matches the account: 401, so the gateway drops the cookie and the client
// goes to sign-in.
func writeSessionRevoked(w http.ResponseWriter) {
	w.Header().Set(headerSessionRevoked, "1")
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session_revoked"})
}

// userPrincipal resolves the request's user principal: the one the gateway
// attached in-process, else one the listener's edge resolves (edgePrincipal).
//
// Returns (nil, false) when the request carries neither — the caller should
// try its other auth paths (the admin key, scoped API keys). Returns
// handled=true when it wrote the response on rejection (user == nil) or
// resolved the member (user != nil), with the resolved principal on the
// returned request's context. Shared by AdminOrUserAuthMiddleware,
// authorizeTaskCreator and HandleUpload so the fail-closed semantics cannot
// drift between them.
//
// Order of verdicts: membership outranks the session (a deleted account gets
// the 403 the UI keys on, not a "sign in again" it cannot act on), and a
// lookup failure is a 500, never a revocation — an unreachable database says
// nothing about the session.
func (h *Handlers) userPrincipal(w http.ResponseWriter, r *http.Request) (*models.User, *http.Request, bool) {
	p, ok := authn.From(r.Context())
	if !ok {
		var handled bool
		if p, handled = h.edgePrincipal(w, r); !handled {
			return nil, r, false
		}
		if p.AuthMethod == "" {
			return nil, r, true // refused; the edge wrote the response
		}
	}
	if p.Email == "" {
		writeError(w, http.StatusBadRequest, "missing user identity")
		return nil, r, true
	}
	promptLibrary := isPromptLibraryPath(r.URL.Path)
	var user *models.User
	resolved, _, err := authn.Resolve(r.Context(), h.storage.Identity(), p, func(account *identity.User) bool {
		user = headerTrustMember(account, promptLibrary)
		return user != nil
	})
	switch {
	case errors.Is(err, authn.ErrNotMember):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_member"})
		return nil, r, true
	case errors.Is(err, authn.ErrSessionRevoked):
		writeSessionRevoked(w)
		return nil, r, true
	case err != nil:
		writeError(w, http.StatusInternalServerError, "Membership check failed")
		return nil, r, true
	}
	if refuseReadonlyUserMutation(w, r, user) {
		return nil, r, true
	}
	ctx := authn.With(r.Context(), resolved)
	ctx = context.WithValue(ctx, userContextKey, user)
	return user, r.WithContext(ctx), true
}
