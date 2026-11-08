// Copyright (c) 2025 ElcanoTek

package handlers

import (
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"time"
)

// SchedRateLimitMiddleware throttles the high-cost orchestrator endpoints
// (POST /tasks, POST /upload) with the shared sliding-window limiter
// (internal/ratelimit) — the SAME implementation the chat server uses, so the
// algorithm is single-sourced. Checks run in order: admin-key bypass →
// per-identity cap → process-wide cap.
//
// Identity is the authenticated API key's stable ID when an X-API-Key is present
// and resolvable (and that key's own RateLimit, when > 0, overrides the
// per-minute default for it); otherwise the client IP (cookie / bearer callers,
// and unknown keys).
//
// The process-wide window is charged only for a request the per-identity
// window admitted, so a request refused per identity never counts against
// anyone else. It is split in two buckets of the same size: one for callers
// that presented a credential (a resolvable API key, the gateway's server
// token, a password-login bearer token, or an identity an auth middleware
// already established), and one for everything else — so unauthenticated
// traffic draws only on its own bucket and never on the budget of
// authenticated callers.
//
// A 429 carries the standard X-RateLimit-* headers, a Retry-After, and a JSON
// body {"error":"rate_limit_exceeded","retry_after_seconds":N}. The existing
// per-key hourly cap in apikeys.ValidateKey is left intact — this is an
// additional, finer-grained gate on the two most expensive endpoints.
func (h *Handlers) SchedRateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The admin key bypasses all limits, mirroring how it bypasses auth. Guard
		// on a configured key so an unset AdminAPIKey can't grant a blanket bypass
		// (verifyAdminKey would otherwise match an empty header against the empty
		// config).
		if h.config.AdminAPIKey != "" && h.verifyAdminKey(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Resolve a per-identity bucket key and the per-minute cap for it.
		perMin := h.config.SchedRateLimitPerMinute
		perDay := h.config.SchedRateLimitPerDay
		identity := ""
		if rawKey := r.Header.Get("X-API-Key"); rawKey != "" {
			if keyID, override, ok := h.apiKeys.LookupKeyMeta(rawKey); ok {
				identity = "key:" + keyID
				if override > 0 {
					perMin = override
				}
			}
		}
		keyed := identity != ""
		if identity == "" {
			// Cookie/bearer callers and unknown keys: throttle by client IP.
			identity = "ip:" + getClientIP(r)
		}

		if ok, retry := h.taskKeyRL.AllowN(identity, perMin, perDay); !ok {
			h.writeRateLimited(w, retry, perMin, "minute")
			return
		}

		// Process-wide cap, charged only once the per-identity window admitted
		// the request, in the bucket for its caller class.
		globalBucket := schedGlobalAnonymousBucket
		if keyed || h.rateLimitCallerAuthenticated(r) {
			globalBucket = schedGlobalAuthenticatedBucket
		}
		if ok, retry := h.taskGlobalRL.Allow(globalBucket); !ok {
			h.writeRateLimited(w, retry, h.config.SchedGlobalRateLimitPerMinute, "global")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The two process-wide buckets SchedRateLimitMiddleware charges.
const (
	schedGlobalAuthenticatedBucket = "__global__"
	schedGlobalAnonymousBucket     = "__global_unauthenticated__"
)

// rateLimitCallerAuthenticated reports whether r carries a credential other
// than an API key (which the caller resolves itself): an identity an auth
// middleware already put on the context, or the web gateway's server token.
// It only chooses a rate-limit bucket; it
// authorizes nothing.
func (h *Handlers) rateLimitCallerAuthenticated(r *http.Request) bool {
	if GetUserFromContext(r.Context()) != nil || GetAPIKeyFromContext(r.Context()) != nil {
		return true
	}
	if tok := r.Header.Get("X-Orchestrator-Server-Token"); tok != "" && h.config.SharedToken != "" &&
		subtle.ConstantTimeCompare([]byte(tok), []byte(h.config.SharedToken)) == 1 {
		return true
	}
	return false
}

// writeRateLimited emits a 429 with the full rate-limit header set and the
// standardized JSON body, and logs the hit with its window.
func (h *Handlers) writeRateLimited(w http.ResponseWriter, retry time.Duration, limit int, window string) {
	retrySeconds := int(retry.Seconds())
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(retry).Unix(), 10))
	w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
	//nolint:gosec // G706: window is a fixed internal label ("global"|"minute"|"day") set by this package, never request input; retrySeconds is an int.
	log.Printf("orchestrator rate limit exceeded (window=%s, retry_after=%ds)", window, retrySeconds)
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":               "rate_limit_exceeded",
		"retry_after_seconds": retrySeconds,
	})
}
