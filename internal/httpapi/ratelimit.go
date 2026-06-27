package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateHitCounter tallies rate-limit 429s by reason ("rpm"|"day"|"concurrent").
// Held behind a pointer on Server so a Server value stays copyable. A Prometheus
// surface is deferred to the metrics issue (#176); RateLimitHits exposes a
// snapshot for dashboard/admin use until then.
type rateHitCounter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func newRateHitCounter() *rateHitCounter {
	return &rateHitCounter{counts: map[string]int64{}}
}

func (c *rateHitCounter) inc(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.counts[reason]++
	c.mu.Unlock()
}

func (c *rateHitCounter) snapshot() map[string]int64 {
	if c == nil {
		return map[string]int64{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

// RateLimitHits returns a snapshot of the chat rate-limit 429 tallies by reason.
func (s *Server) RateLimitHits() map[string]int64 { return s.rateLimitHits.snapshot() }

// rateLimitMiddleware guards /chat with the shared sliding-window limiter
// (internal/ratelimit), keyed by user email. Applied only to /chat because the
// other chat endpoints are cheap and shouldn't block the UI on a limit hit.
//
// Admins (ADMIN_EMAILS) are exempt. Every non-admin response — success or 429 —
// carries advisory X-RateLimit-Limit/Remaining/Reset headers; a 429 adds
// Retry-After and a JSON body. The concurrent-turn cap is enforced separately in
// postChat (its slot spans the turn goroutine, not the HTTP handler).
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rate == nil { // rate limiting disabled
			next.ServeHTTP(w, r)
			return
		}
		user := userFromCtx(r.Context())
		if s.isAdmin(user) { // admins are exempt
			next.ServeHTTP(w, r)
			return
		}

		ok, retry := s.rate.Allow(user)
		limit, remaining, reset := s.rate.Snapshot(user)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		if !ok {
			reason := "rpm"
			if retry > time.Minute {
				reason = "day"
			}
			s.rateLimitHits.inc(reason)
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{
				"error":               "rate_limit_exceeded",
				"reason":              reason,
				"retry_after_seconds": int(retry.Seconds()),
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// admitConcurrentTurn enforces the per-user concurrent-turn cap. It returns a
// release func (call when the turn completes) and admitted=false after writing a
// 429 when the user is already at the limit. Admins and a disabled limiter are
// always admitted with a no-op release.
func (s *Server) admitConcurrentTurn(w http.ResponseWriter, user string) (release func(), admitted bool) {
	if s.concurrent == nil || s.isAdmin(user) {
		return func() {}, true
	}
	if !s.concurrent.Acquire(user) {
		s.rateLimitHits.inc("concurrent")
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{
			"error":  "Too many concurrent turns",
			"active": s.concurrent.Active(user),
			"limit":  s.concurrent.Limit(),
		})
		return nil, false
	}
	return func() { s.concurrent.Release(user) }, true
}

// writeJSONStatus writes v as JSON with an explicit status code. (writeJSON
// defaults to 200; this is its status-carrying sibling for error bodies.)
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}
