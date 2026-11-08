// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/principal"
)

// edgePrincipal resolves the credential a request presented on the chat
// listener's own port, for a request the gateway did not attach a principal
// to. It writes the refusal itself and reports false.
//
// The one credential it knows is the legacy internal service token: a caller
// holding FLEET_SERVER_TOKEN (`fleet chat`, `fleet acp`, fleet-bench, all
// on the box) names its user in X-User-Email. The token can already assert
// any identity, so the header adds no spoofing surface; Caddy strips both on
// the one chat path it exposes (/webhooks/*, ADR-0053). Nothing else is read
// from headers: the session-epoch claim only ever came from the gateway,
// which now hands it over in the principal.
func (s *Server) edgePrincipal(w http.ResponseWriter, r *http.Request) (principal.Principal, bool) {
	tok := r.Header.Get("X-Chat-Server-Token")
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.sharedToken)) != 1 || s.sharedToken == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return principal.Principal{}, false
	}
	return principal.Principal{
		Email:      strings.ToLower(strings.TrimSpace(r.Header.Get("X-User-Email"))),
		AuthMethod: principal.MethodServiceToken,
	}, true
}
