// Copyright (c) 2025 ElcanoTek

package handlers

import (
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateLimitCounter is a tiny concurrency-safe tally of 429s by window. Held
// behind a pointer on Handlers so a Handlers value remains copyable (govet
// copylocks).
type rateLimitCounter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func newRateLimitCounter() *rateLimitCounter {
	return &rateLimitCounter{counts: map[string]int64{}}
}

func (c *rateLimitCounter) inc(window string) {
	c.mu.Lock()
	c.counts[window]++
	c.mu.Unlock()
}

func (c *rateLimitCounter) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// SchedRateLimitMiddleware throttles the high-cost orchestrator endpoints
// (POST /tasks, POST /upload) with the shared sliding-window limiter
// (internal/ratelimit) — the SAME implementation the chat server uses, so the
// algorithm is single-sourced. Checks run in order: admin-key bypass → global
// cap → per-identity cap.
//
// Identity is the authenticated API key's stable ID when an X-API-Key is present
// and resolvable (and that key's own RateLimit, when > 0, overrides the
// per-minute default for it); otherwise the client IP (cookie / bearer callers,
// and unknown keys). The global window is a single process-wide counter so even
// a fleet of legitimate keys can't collectively overwhelm the box.
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

		// Global cap first — process-wide across every caller.
		if ok, retry := h.taskGlobalRL.Allow("__global__"); !ok {
			h.writeRateLimited(w, retry, h.config.SchedGlobalRateLimitPerMinute, "global")
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
		if identity == "" {
			// Cookie/bearer callers and unknown keys: throttle by client IP.
			identity = "ip:" + getClientIP(r)
		}

		if ok, retry := h.taskKeyRL.AllowN(identity, perMin, perDay); !ok {
			h.writeRateLimited(w, retry, perMin, "minute")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeRateLimited emits a 429 with the full rate-limit header set and the
// standardized JSON body, and records the hit by window.
func (h *Handlers) writeRateLimited(w http.ResponseWriter, retry time.Duration, limit int, window string) {
	retrySeconds := int(retry.Seconds())
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(retry).Unix(), 10))
	w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
	if h.taskRLCounter != nil {
		h.taskRLCounter.inc(window)
	}
	//nolint:gosec // G706: window is a fixed internal label ("global"|"minute"|"day") set by this package, never request input; retrySeconds is an int.
	log.Printf("orchestrator rate limit exceeded (window=%s, retry_after=%ds)", window, retrySeconds)
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":               "rate_limit_exceeded",
		"retry_after_seconds": retrySeconds,
	})
}

// RateLimitExceededCounts returns a snapshot of the 429 tallies by window. A
// Prometheus surface is deferred to the dedicated metrics issue (#176); until
// then these counts can be surfaced via dashboard stats.
func (h *Handlers) RateLimitExceededCounts() map[string]int64 {
	if h.taskRLCounter == nil {
		return map[string]int64{}
	}
	return h.taskRLCounter.snapshot()
}
