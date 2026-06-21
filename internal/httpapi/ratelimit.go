package httpapi

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateLimiter implements a per-user token bucket for the /chat endpoint.
// Scope is wide enough (one bucket per email across all their sessions)
// to prevent a single user from draining OpenRouter budget, narrow enough
// that day-to-day heavy use doesn't trip it.
//
// Defaults: 40 turns/min (generous for a power user mid-analysis),
// 2000/day (hard cap to catch runaway loops — not something a human will
// hit in normal use). Tune via CHAT_RATE_PER_MIN + CHAT_RATE_PER_DAY.
// Set either to 0 to disable that window; set both to 0 to disable
// rate limiting entirely.
type rateLimiter struct {
	mu sync.RWMutex

	perMinute int
	perDay    int

	// Sliding window counts, keyed by user_email. We accept a little memory
	// overhead in exchange for precise day-boundary behavior without cron.
	users map[string]*userBucket

	// lastSweep gates the idle-bucket janitor so the map can't grow
	// without bound over a long-lived process.
	lastSweep int64
}

// sweepInterval is how often allow() prunes buckets whose newest sample
// has aged out of the day window. Buckets are tiny, so this only matters
// over months of uptime — but a map that only ever grows is still a leak.
const sweepInterval = 6 * 60 * 60 // seconds

type userBucket struct {
	mu               sync.Mutex
	minuteTimestamps []int64 // unix seconds of recent requests (rolling window)
	dayTimestamps    []int64
}

func newRateLimiter(perMinute, perDay int) *rateLimiter {
	return &rateLimiter{
		perMinute: perMinute,
		perDay:    perDay,
		users:     map[string]*userBucket{},
	}
}

// allow returns true if the request should proceed, plus a Retry-After
// duration if blocked.
func (r *rateLimiter) allow(email string) (bool, time.Duration) {
	if r == nil || (r.perMinute <= 0 && r.perDay <= 0) {
		return true, 0
	}

	r.mu.RLock()
	b, ok := r.users[email]
	r.mu.RUnlock()

	if !ok {
		r.mu.Lock()
		b, ok = r.users[email]
		if !ok {
			b = &userBucket{}
			r.users[email] = b
		}
		r.mu.Unlock()
	}

	r.maybeSweep()

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().Unix()

	// Trim older samples.
	minuteAgo := now - 60
	dayAgo := now - 86400
	b.minuteTimestamps = dropBefore(b.minuteTimestamps, minuteAgo)
	b.dayTimestamps = dropBefore(b.dayTimestamps, dayAgo)

	if r.perMinute > 0 && len(b.minuteTimestamps) >= r.perMinute {
		// Oldest sample in the window drops off after (60 - (now - oldest)) seconds.
		retry := 60 - (now - b.minuteTimestamps[0])
		if retry < 1 {
			retry = 1
		}
		return false, time.Duration(retry) * time.Second
	}
	if r.perDay > 0 && len(b.dayTimestamps) >= r.perDay {
		retry := 86400 - (now - b.dayTimestamps[0])
		if retry < 1 {
			retry = 1
		}
		return false, time.Duration(retry) * time.Second
	}

	b.minuteTimestamps = append(b.minuteTimestamps, now)
	b.dayTimestamps = append(b.dayTimestamps, now)
	return true, 0
}

// maybeSweep prunes buckets whose newest sample is older than the day
// window, at most once per sweepInterval. Runs under the map write lock;
// with the small provisioned-user population this is microseconds.
func (r *rateLimiter) maybeSweep() {
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	if now-r.lastSweep < sweepInterval {
		return
	}
	r.lastSweep = now
	cutoff := now - 86400
	for email, b := range r.users {
		b.mu.Lock()
		n := len(b.dayTimestamps)
		// An empty bucket is NOT idle — it only exists in the moment
		// between creation and its first append in allow(); deleting it
		// here would orphan the caller's pointer and skip its count.
		idle := n > 0 && b.dayTimestamps[n-1] < cutoff
		b.mu.Unlock()
		if idle {
			delete(r.users, email)
		}
	}
}

// dropBefore returns ts trimmed of all values < cutoff. The slice is
// already sorted by insertion order, so a linear scan works. The trim
// is performed IN PLACE on ts's backing array — safe here because the
// only caller is rateLimiter.allow under b.mu, which is the sole owner
// of the slice header. Skipping the per-call allocation drops dropBefore
// from ~5000 ns/op to ~1000 ns/op on a 2000-element backing array (see
// BenchmarkRateLimiterAllow).
func dropBefore(ts []int64, cutoff int64) []int64 {
	i := 0
	for i < len(ts) && ts[i] < cutoff {
		i++
	}
	if i == 0 {
		return ts
	}
	n := copy(ts, ts[i:])
	return ts[:n]
}

// rateLimitMiddleware guards /chat. Applied only to /chat because the
// other endpoints are cheap and shouldn't block the UI on limit-hit.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.rate == nil {
			next.ServeHTTP(w, r)
			return
		}
		user := userFromCtx(r.Context())
		ok, retry := s.rate.allow(user)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
			http.Error(w, "rate limit exceeded — slow down and try again shortly",
				http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
