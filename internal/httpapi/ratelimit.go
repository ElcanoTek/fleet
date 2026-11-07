package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ElcanoTek/fleet/internal/metrics"
)

// rateLimitMiddleware guards /chat and /attachments with the shared
// sliding-window limiter (internal/ratelimit), keyed by user email.
// Other chat endpoints are cheap; /attachments is the other high-cost
// write (up to 2× UploadMaxBytes per request) so it shares the window
// (#1112).
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
		// Deliberately the ADMIN_EMAILS allowlist only (isAdmin), not
		// callerIsAdmin: a users.role = 'admin' account is rate-limited like
		// any member.
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
			metrics.RecordChatRateLimited(reason)
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
	// Deliberately the ADMIN_EMAILS allowlist only (isAdmin), not
	// callerIsAdmin: a users.role = 'admin' account gets the same cap as any
	// member.
	if s.concurrent == nil || s.isAdmin(user) {
		return func() {}, true
	}
	if !s.concurrent.Acquire(user) {
		metrics.RecordChatRateLimited("concurrent")
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
	setJSONContentType(w)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json failed", "err", err)
	}
}
