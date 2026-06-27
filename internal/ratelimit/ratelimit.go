// Package ratelimit is the single sliding-window rate-limiter implementation
// shared by the chat server (per-user /chat throttling) and the orchestrator
// (per-API-key + global /tasks throttling). There is deliberately ONE copy of
// the algorithm: both servers import this package rather than maintaining
// parallel implementations that could drift.
//
// A Limiter keys requests by an arbitrary string (user email, API key ID,
// client IP, or a fixed "__global__" sentinel) and enforces two independent
// rolling windows — per minute and per day. Either window set to 0 disables it;
// both at 0 disables the limiter entirely.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a per-key sliding-window rate limiter. The zero value is not
// usable; construct with New. Safe for concurrent use.
type Limiter struct {
	mu sync.RWMutex

	perMinute int
	perDay    int

	// Sliding window counts, keyed by the caller's chosen identity string. We
	// accept a little memory overhead in exchange for precise window behavior
	// without a cron.
	keys map[string]*bucket

	// lastSweep gates the idle-bucket janitor so the map can't grow without
	// bound over a long-lived process.
	lastSweep int64
}

// sweepInterval is how often Allow prunes buckets whose newest sample has aged
// out of the day window. Buckets are tiny, so this only matters over months of
// uptime — but a map that only ever grows is still a leak.
const sweepInterval = 6 * 60 * 60 // seconds

type bucket struct {
	mu               sync.Mutex
	minuteTimestamps []int64 // unix seconds of recent requests (rolling window)
	dayTimestamps    []int64
}

// New returns a Limiter enforcing perMinute requests/minute and perDay
// requests/day per key. A window with a non-positive bound is disabled.
func New(perMinute, perDay int) *Limiter {
	return &Limiter{
		perMinute: perMinute,
		perDay:    perDay,
		keys:      map[string]*bucket{},
	}
}

// PerMinute reports the configured per-minute bound (0 = disabled). Callers use
// it to populate the X-RateLimit-Limit header.
func (l *Limiter) PerMinute() int {
	if l == nil {
		return 0
	}
	return l.perMinute
}

// Allow records and authorizes a request for key against the limiter's
// configured per-minute/per-day bounds. It returns false plus a Retry-After
// duration when either window is full. A nil limiter, or one with both windows
// disabled, always allows.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	return l.AllowN(key, l.perMinute, l.perDay)
}

// AllowN is Allow with caller-supplied per-call bounds, overriding the instance
// defaults. It exists for callers whose cap varies per key — e.g. an API key
// whose own rate_limit overrides the global default. The per-key bucket storage
// is shared with Allow; only the thresholds checked on THIS call differ. A
// non-positive bound disables that window for the call.
func (l *Limiter) AllowN(key string, perMinute, perDay int) (bool, time.Duration) {
	if l == nil || (perMinute <= 0 && perDay <= 0) {
		return true, 0
	}

	l.mu.RLock()
	b, ok := l.keys[key]
	l.mu.RUnlock()

	if !ok {
		l.mu.Lock()
		b, ok = l.keys[key]
		if !ok {
			b = &bucket{}
			l.keys[key] = b
		}
		l.mu.Unlock()
	}

	l.maybeSweep()

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().Unix()

	// Trim older samples.
	minuteAgo := now - 60
	dayAgo := now - 86400
	b.minuteTimestamps = dropBefore(b.minuteTimestamps, minuteAgo)
	b.dayTimestamps = dropBefore(b.dayTimestamps, dayAgo)

	if perMinute > 0 && len(b.minuteTimestamps) >= perMinute {
		// Oldest sample in the window drops off after (60 - (now - oldest)) seconds.
		retry := 60 - (now - b.minuteTimestamps[0])
		if retry < 1 {
			retry = 1
		}
		return false, time.Duration(retry) * time.Second
	}
	if perDay > 0 && len(b.dayTimestamps) >= perDay {
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

// maybeSweep prunes buckets whose newest sample is older than the day window, at
// most once per sweepInterval. Runs under the map write lock; with a small key
// population this is microseconds.
func (l *Limiter) maybeSweep() {
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now-l.lastSweep < sweepInterval {
		return
	}
	l.lastSweep = now
	cutoff := now - 86400
	for key, b := range l.keys {
		b.mu.Lock()
		n := len(b.dayTimestamps)
		// An empty bucket is NOT idle — it only exists in the moment between
		// creation and its first append in Allow; deleting it here would orphan
		// the caller's pointer and skip its count.
		idle := n > 0 && b.dayTimestamps[n-1] < cutoff
		b.mu.Unlock()
		if idle {
			delete(l.keys, key)
		}
	}
}

// dropBefore returns ts trimmed of all values < cutoff. The slice is already
// sorted by insertion order, so a linear scan works. The trim is performed IN
// PLACE on ts's backing array — safe here because the only caller is Allow under
// b.mu, which is the sole owner of the slice header. Skipping the per-call
// allocation keeps the hot path allocation-free on a full backing array.
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
