// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net/http"
)

// retiredServiceTokenHeader is the header of the internal service-token
// channel ADR-0108 removed. Nothing honours it; a caller still sending it
// gets serviceTokenRetired instead of a silent fall-through.
const retiredServiceTokenHeader = "X-Orchestrator-Server-Token"

// serviceTokenRetiredMessage is the 401 body for a caller still using the
// retired channel, naming what replaced it.
const serviceTokenRetiredMessage = "the internal service token (X-Orchestrator-Server-Token, FLEET_SERVER_TOKEN) was removed (ADR-0108): authenticate with an API key in X-API-Key"

// refuseRetiredServiceToken answers 401 for a request that presents the
// retired service token and reports whether it did; on true the caller must
// stop. A request the gateway attached a principal to never reaches here.
func refuseRetiredServiceToken(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(retiredServiceTokenHeader) == "" {
		return false
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "service_token_retired", "message": serviceTokenRetiredMessage})
	return true
}

// RetiredServiceTokenMiddleware refuses, ahead of CSRF and every auth path,
// a request that still presents the retired service token, so the caller
// gets the 401 that names the replacement rather than a CSRF 403 or an
// unexplained "Unauthorized". The gateway's in-process calls carry a
// principal and no such header.
func (h *Handlers) RetiredServiceTokenMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuseRetiredServiceToken(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
