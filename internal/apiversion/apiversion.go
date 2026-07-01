// Package apiversion adds a versioned (/v1) HTTP surface over fleet's existing
// bare-path routers, plus version discovery (#321), WITHOUT touching any route
// registration.
//
// Router wraps an already-built handler: a /v1/<path> request is served by the
// same handler at <path> with an X-Fleet-API-Version header, and a legacy
// (bare-path) request is served unchanged but tagged with Deprecation/Link
// headers so integrations get an auditable migration signal. Health probes and
// the version-info endpoint are unversioned forever and never tagged.
//
// Because the wrapper strips /v1 and delegates to the SAME inner handler, no
// route is registered twice: the OpenAPI route-parity test still walks the bare
// router, and clients read the /v1 base from the spec's servers block.
package apiversion

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Version is the HTTP API major version, surfaced in the X-Fleet-API-Version
// response header and by InfoHandler. It increments ONLY on a breaking API
// change (see docs/api-versioning.md) — independent of the binary's semver.
const Version = "1"

// VersionHeader is the response header carrying Version on /v1 responses.
const VersionHeader = "X-Fleet-API-Version"

// prefix is the versioned path prefix.
const prefix = "/v1"

// Router wraps inner so both the versioned (/v1/...) and legacy (bare) paths
// work. Health probes + /api-info are unversioned forever (never deprecation-
// tagged). It registers no routes; it only rewrites the path + sets headers.
func Router(inner http.Handler) http.Handler {
	// StripPrefix delegates /v1/<path> to inner at <path>; wrap it to also set
	// the version header before the inner handler writes.
	versioned := http.StripPrefix(prefix, headerSetter(inner, VersionHeader, Version))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			versioned.ServeHTTP(w, r)
			return
		}
		// Legacy bare path: signal deprecation so integrations can migrate,
		// except for endpoints that are intentionally unversioned forever.
		if !unversionedForever(r.URL.Path) {
			w.Header().Set("Deprecation", "true")
			w.Header().Set("Link", "<"+prefix+r.URL.Path+`>; rel="successor-version"`)
		}
		inner.ServeHTTP(w, r)
	})
}

// unversionedForever reports whether a bare path is deliberately not versioned
// (and so must NOT carry a deprecation signal): liveness/readiness probes and
// the version-discovery endpoint.
func unversionedForever(path string) bool {
	switch path {
	case "/healthz", "/health", "/readyz", "/api-info":
		return true
	}
	return false
}

// headerSetter returns a handler that sets header k=v before delegating to h.
func headerSetter(h http.Handler, k, v string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(k, v)
		h.ServeHTTP(w, r)
	})
}

// InfoHandler serves GET /api-info (and /v1/api-info): machine-readable version
// metadata so a client can assert compatibility at startup. Unauthenticated,
// same posture as the health probe. fleetVersion is the binary semver.
func InfoHandler(fleetVersion string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"api_version":         Version,
			"fleet_version":       fleetVersion,
			"supported_versions":  []string{Version},
			"deprecated_versions": []string{},
			"schema_url":          "https://github.com/ElcanoTek/fleet/blob/main/docs/openapi.yaml",
		})
	}
}
