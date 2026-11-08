// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"crypto/subtle"
	"net/http"
	"strings"

	authn "github.com/ElcanoTek/fleet/internal/principal"
)

// edgePrincipal resolves the legacy internal service token on the
// orchestrator listener's own port, for a request the gateway did not attach
// a principal to: a caller holding FLEET_SERVER_TOKEN names its user in
// X-User-Email. The token can already assert any identity, so the header adds
// no spoofing surface, and Caddy strips both on every path it exposes
// (ADR-0053). It is fail-closed: a PRESENT-but-wrong token is refused outright
// (no fall-through to the weaker key paths).
//
// handled=false: no token, try the other auth paths. handled=true with an
// empty AuthMethod: refused, response written.
func (h *Handlers) edgePrincipal(w http.ResponseWriter, r *http.Request) (authn.Principal, bool) {
	tok := r.Header.Get("X-Orchestrator-Server-Token")
	if tok == "" {
		return authn.Principal{}, false
	}
	if h.config.SharedToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(h.config.SharedToken)) != 1 {
		writeError(w, http.StatusForbidden, "Forbidden")
		return authn.Principal{}, true
	}
	return authn.Principal{
		Email:      strings.ToLower(strings.TrimSpace(r.Header.Get("X-User-Email"))),
		AuthMethod: authn.MethodServiceToken,
	}, true
}

// edgeServiceToken reports whether r carries the valid internal service
// token (a rate-limit bucket choice and the CSRF exemption only; it
// authorizes nothing).
func (h *Handlers) edgeServiceToken(r *http.Request) bool {
	tok := r.Header.Get("X-Orchestrator-Server-Token")
	return tok != "" && h.config.SharedToken != "" &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(h.config.SharedToken)) == 1
}
