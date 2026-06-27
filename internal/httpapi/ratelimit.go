package httpapi

import (
	"net/http"
	"strconv"
)

// rateLimitMiddleware guards /chat with the shared sliding-window limiter
// (internal/ratelimit), keyed by user email. Applied only to /chat because the
// other chat endpoints are cheap and shouldn't block the UI on a limit hit.
//
// Defaults: 40 turns/min, 2000/day (see config CHAT_RATE_PER_MIN /
// CHAT_RATE_PER_DAY). Either window at 0 disables it; both at 0 disables rate
// limiting entirely.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rate == nil {
			next.ServeHTTP(w, r)
			return
		}
		user := userFromCtx(r.Context())
		ok, retry := s.rate.Allow(user)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
			http.Error(w, "rate limit exceeded — slow down and try again shortly",
				http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
