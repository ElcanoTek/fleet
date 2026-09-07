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
	"math"
	"sync"
	"sync/atomic"
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
	// bound over a long-lived process. Atomic so the once-per-6h check on the
	// hot path is a plain load, not a write-lock acquisition on every AllowN.
	lastSweep atomic.Int64
}

// sweepInterval is how often Allow prunes buckets whose newest sample has aged
// out of the day window. Buckets are tiny, so this only matters over months of
// uptime — but a map that only ever grows is still a leak.
const sweepInterval = 6 * 60 * 60 // seconds

type bucket struct {
	mu               sync.Mutex
	minuteTimestamps []int64 // unix seconds of recent requests (rolling window)
	dayTimestamps    []int64
	// evicted is set (under mu) by the sweep the moment it deletes this bucket
	// from the map. An AllowN that fetched the pointer before the delete sees
	// it on lock and re-fetches instead of appending to an orphan — the lost
	// update that would otherwise let one request through uncounted.
	evicted bool
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

// Keys reports how many per-key buckets the limiter currently tracks. It exists
// for tests asserting the map is not attacker-growable: a caller keying buckets
// on an unauthenticated, attacker-chosen string would let a flood of distinct
// values grow the map until the sweep (up to 24h), so callers must only Allow a
// key AFTER it has been validated — and a test can prove that by checking Keys
// stays at zero across a burst of invalid identities.
func (l *Limiter) Keys() int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.keys)
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

	// Sweep BEFORE fetching the bucket so this call's own sweep can never
	// delete the bucket it is about to count into. A concurrent caller's sweep
	// still can; the evicted flag below closes that window.
	l.maybeSweep()

	b := l.bucketFor(key)
	b.mu.Lock()
	for b.evicted {
		// The sweep won the race between our fetch and our lock: this bucket
		// is no longer in the map. Counting into it would be a lost update, so
		// take (or create) the live one instead.
		b.mu.Unlock()
		b = l.bucketFor(key)
		b.mu.Lock()
	}
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

// bucketFor returns key's live bucket, creating it under the write lock when
// absent (double-checked so the common path is a read lock).
func (l *Limiter) bucketFor(key string) *bucket {
	l.mu.RLock()
	b, ok := l.keys[key]
	l.mu.RUnlock()
	if ok {
		return b
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok = l.keys[key]; !ok {
		b = &bucket{}
		l.keys[key] = b
	}
	return b
}

// Snapshot reports the per-minute limit, the remaining allowance, and the unix
// time the minute window resets for key — WITHOUT recording a request. Used to
// emit advisory X-RateLimit-* headers on a response. remaining is relative to
// the configured per-minute bound; reset is now+60 when the window is empty,
// else (oldest sample + 60). A caller that admitted the request through AllowN
// with a per-key bound must use SnapshotN with that same bound, or the headers
// advertise the instance default rather than the cap actually enforced.
func (l *Limiter) Snapshot(key string) (limit, remaining int, reset int64) {
	if l == nil {
		return 0, 0, time.Now().Unix() + 60
	}
	return l.SnapshotN(key, l.perMinute)
}

// SnapshotN is Snapshot against a caller-supplied per-minute bound — the
// read-side twin of AllowN, so a key with its own rate_limit gets headers that
// match the ceiling it was admitted under. A non-positive bound reports the
// window as disabled (0, 0, now+60).
func (l *Limiter) SnapshotN(key string, perMinute int) (limit, remaining int, reset int64) {
	now := time.Now().Unix()
	if l == nil || perMinute <= 0 {
		return 0, 0, now + 60
	}
	limit = perMinute
	reset = now + 60

	l.mu.RLock()
	b, ok := l.keys[key]
	l.mu.RUnlock()
	if !ok {
		return limit, limit, reset
	}

	b.mu.Lock()
	b.minuteTimestamps = dropBefore(b.minuteTimestamps, now-60)
	used := len(b.minuteTimestamps)
	if used > 0 {
		reset = b.minuteTimestamps[0] + 60
	}
	b.mu.Unlock()

	remaining = limit - used
	if remaining < 0 {
		remaining = 0
	}
	return limit, remaining, reset
}

// ConcurrencyLimiter caps the number of simultaneously-active operations per
// key (e.g. in-flight chat turns per user). Unlike Limiter it tracks live
// occupancy, not a time window: callers Acquire before starting work and Release
// when it completes. Safe for concurrent use.
//
// Occupancy lives in a plain mutex-guarded map rather than a sync.Map of
// atomics: eviction is what matters here. Release deletes a key the moment its
// count hits zero, so the map is bounded by the number of keys with work
// IN FLIGHT — not, as before #594, by every key ever seen (a slow leak on a
// long-lived, high-user-churn process). A lock-free counter can't delete-on-zero
// without racing a concurrent Acquire that holds the about-to-be-orphaned
// counter; doing both transitions under one small lock makes that race
// impossible, and Acquire/Release run once per chat turn, so contention is nil.
type ConcurrencyLimiter struct {
	limit int32

	mu     sync.Mutex
	counts map[string]int32
}

// NewConcurrencyLimiter returns a limiter allowing up to limit concurrent
// operations per key. A non-positive limit disables the cap (Acquire always
// succeeds).
func NewConcurrencyLimiter(limit int) *ConcurrencyLimiter {
	// Clamp to a sane int32 range — limit is a small operator-set cap, never
	// anywhere near the boundary, but bound the conversion explicitly so the
	// overflow checker is satisfied. The conversion is performed inside an
	// explicit range check (rather than a reassigning clamp) because that is
	// the form CodeQL's go/incorrect-integer-conversion recognizes as a barrier.
	capped := int32(0)
	if limit > 0 {
		if limit > math.MaxInt32 {
			capped = math.MaxInt32
		} else {
			capped = int32(limit)
		}
	}
	return &ConcurrencyLimiter{limit: capped, counts: map[string]int32{}}
}

// Acquire reserves a slot for key, returning false when key is already at the
// limit. A disabled limiter (limit <= 0) or nil receiver always succeeds.
func (c *ConcurrencyLimiter) Acquire(key string) bool {
	if c == nil || c.limit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.counts[key] // missing key reads as 0 — no entry is created by the lookup
	if cur >= c.limit {
		return false
	}
	c.counts[key] = cur + 1
	return true
}

// Release frees a slot previously taken by Acquire. It never drops below zero,
// and it evicts the key's entry the moment the count reaches zero so the map
// only holds keys with in-flight work (#594).
func (c *ConcurrencyLimiter) Release(key string) {
	if c == nil || c.limit <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.counts[key]
	if !ok {
		return
	}
	if cur <= 1 {
		delete(c.counts, key)
		return
	}
	c.counts[key] = cur - 1
}

// Active reports the current occupancy for key.
func (c *ConcurrencyLimiter) Active(key string) int32 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

// Limit reports the configured per-key concurrency cap.
func (c *ConcurrencyLimiter) Limit() int32 {
	if c == nil {
		return 0
	}
	return c.limit
}

// maybeSweep prunes buckets whose newest sample is older than the day window, at
// most once per sweepInterval. The interval check is an atomic load so the
// hot path (every AllowN) never takes the map write lock just to learn that no
// sweep is due; only the one caller that wins the CAS pays for the sweep, under
// the write lock — with a small key population that is microseconds.
func (l *Limiter) maybeSweep() {
	now := time.Now().Unix()
	last := l.lastSweep.Load()
	if now-last < sweepInterval || !l.lastSweep.CompareAndSwap(last, now) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now - 86400
	for key, b := range l.keys {
		b.mu.Lock()
		n := len(b.dayTimestamps)
		// An empty bucket is NOT idle — it only exists in the moment between
		// creation and its first append in Allow; deleting it here would orphan
		// the caller's pointer and skip its count.
		idle := n > 0 && b.dayTimestamps[n-1] < cutoff
		if idle {
			// Flag under b.mu so an AllowN holding a pre-delete pointer sees
			// the eviction when it locks, and re-fetches instead of counting
			// into an orphan.
			b.evicted = true
			delete(l.keys, key)
		}
		b.mu.Unlock()
	}
}

// dropBefore returns ts trimmed of all values < cutoff. The slice is already
// sorted by insertion order, so a linear scan works. The trim is performed IN
// PLACE on ts's backing array — safe because every caller (AllowN and
// SnapshotN) holds b.mu, which is the sole owner of the slice header. Skipping
// the per-call allocation keeps the hot path allocation-free on a full backing
// array.
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
