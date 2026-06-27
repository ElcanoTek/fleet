package agentcore

import (
	"log"
	"sync"
	"time"
)

// Provider health / circuit breaker (#267).
//
// Fleet routes every LLM call through OpenRouter. When a model is degraded
// (sustained 429/503), the per-run retry budget alone is too slow: each run
// starts fresh and burns up to maxAttempts×escalations failing calls before
// swapping to the fallback. The circuit breaker accumulates errors ACROSS runs
// per model slug so that once a model is known-bad, subsequent runs skip it and
// go straight to the fallback — and operators get a queryable health view.
//
// State machine per slug:
//
//	closed ──(≥openThreshold errors / window)──► open ──(cooldown)──► half-open
//	  ▲                                                                   │
//	  └────────────────(probe succeeds)──────────────────────────────────┘
//	                                          (probe fails) ─► open (reset timer)

// CircuitState is a model's circuit-breaker state.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // normal: requests flow to the model
	CircuitOpen                         // failing: skip the model, use fallback
	CircuitHalfOpen                     // probing: allow exactly one trial request
)

// String renders the state for JSON / logs.
func (s CircuitState) String() string {
	switch s {
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

const (
	// circuitWindow is the rolling window over which errors accumulate.
	circuitWindow = 5 * time.Minute
	// circuitOpenThreshold is the error count within the window that trips the breaker.
	circuitOpenThreshold = 5
	// circuitCooldown is how long the breaker stays open before allowing a probe.
	circuitCooldown = 60 * time.Second
)

// ModelHealth is a point-in-time snapshot of one model's circuit.
type ModelHealth struct {
	Slug          string       `json:"slug"`
	State         string       `json:"state"`
	RecentErrors  int          `json:"recent_errors"`
	LastError     string       `json:"last_error,omitempty"`
	LastErrorAt   *time.Time   `json:"last_error_at,omitempty"`
	OpenedAt      *time.Time   `json:"opened_at,omitempty"`
	TotalRequests int64        `json:"total_requests"`
	TotalErrors   int64        `json:"total_errors"`
	stateEnum     CircuitState `json:"-"`
}

// modelCircuit is the mutable per-slug state. Guarded by the registry mutex.
type modelCircuit struct {
	state         CircuitState
	errorTimes    []time.Time // error timestamps within the rolling window
	lastError     string
	lastErrorAt   time.Time
	openedAt      time.Time
	totalRequests int64
	totalErrors   int64
	openLogged    bool // whether the sustained-open warning has fired
}

// ProviderHealthRegistry tracks per-model circuit state across runs. Safe for
// concurrent use. A nil registry is a no-op (State==closed, Record* ignored), so
// callers needn't nil-check.
type ProviderHealthRegistry struct {
	mu     sync.Mutex
	models map[string]*modelCircuit
	now    func() time.Time // injectable clock for tests
}

// NewProviderHealthRegistry returns an empty registry using the wall clock.
func NewProviderHealthRegistry() *ProviderHealthRegistry {
	return &ProviderHealthRegistry{
		models: make(map[string]*modelCircuit),
		now:    time.Now,
	}
}

func (r *ProviderHealthRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// circuitFor returns (creating) the slug's circuit. Caller holds r.mu.
func (r *ProviderHealthRegistry) circuitFor(slug string) *modelCircuit {
	c := r.models[slug]
	if c == nil {
		c = &modelCircuit{state: CircuitClosed}
		r.models[slug] = c
	}
	return c
}

// trimWindow drops error timestamps older than the rolling window.
func (c *modelCircuit) trimWindow(now time.Time) {
	cutoff := now.Add(-circuitWindow)
	i := 0
	for i < len(c.errorTimes) && c.errorTimes[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		c.errorTimes = append(c.errorTimes[:0], c.errorTimes[i:]...)
	}
}

// State returns the current circuit state for slug, applying the cooldown
// transition (open → half-open) lazily. A nil registry reports closed.
func (r *ProviderHealthRegistry) State(slug string) CircuitState {
	if r == nil {
		return CircuitClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.circuitFor(slug)
	now := r.clock()
	if c.state == CircuitOpen && now.Sub(c.openedAt) >= circuitCooldown {
		c.state = CircuitHalfOpen
	}
	return c.state
}

// RecordSuccess records a successful call: it closes a half-open (or open-past-
// cooldown) circuit and clears the error window. A nil registry is a no-op.
func (r *ProviderHealthRegistry) RecordSuccess(slug string) {
	if r == nil || slug == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.circuitFor(slug)
	c.totalRequests++
	c.state = CircuitClosed
	c.errorTimes = c.errorTimes[:0]
	c.openLogged = false
}

// RecordError records a provider error and returns the resulting state. The
// circuit opens once errorThreshold errors land within the window; a failure
// while half-open re-opens it. msg is a short error description for the
// operator-facing snapshot. A nil registry is a no-op (returns closed).
func (r *ProviderHealthRegistry) RecordError(slug, msg string) CircuitState {
	if r == nil || slug == "" {
		return CircuitClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.circuitFor(slug)
	now := r.clock()
	c.totalRequests++
	c.totalErrors++
	c.lastError = msg
	c.lastErrorAt = now

	if c.state == CircuitHalfOpen {
		// The probe failed — re-open and reset the cooldown.
		c.state = CircuitOpen
		c.openedAt = now
		logCircuitOpen(slug, c)
		return c.state
	}

	c.errorTimes = append(c.errorTimes, now)
	c.trimWindow(now)
	if c.state == CircuitClosed && len(c.errorTimes) >= circuitOpenThreshold {
		c.state = CircuitOpen
		c.openedAt = now
		c.openLogged = false
		logCircuitOpen(slug, c)
	}
	return c.state
}

// logCircuitOpen emits a machine-readable warning once per open episode so an
// operator's log pipeline (alertmanager / PagerDuty) can page on a degraded
// model. Guarded by openLogged so it fires on the transition, not per error.
func logCircuitOpen(slug string, c *modelCircuit) {
	if c.openLogged {
		return
	}
	c.openLogged = true
	log.Printf(`{"level":"WARN","event":"circuit_open","model":%q,"recent_errors":%d,"last_error":%q}`,
		slug, len(c.errorTimes), c.lastError)
}

// Snapshot returns a point-in-time copy of every tracked model's health,
// applying the lazy cooldown transition. Nil registry returns nil.
func (r *ProviderHealthRegistry) Snapshot() []ModelHealth {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	out := make([]ModelHealth, 0, len(r.models))
	for slug, c := range r.models {
		if c.state == CircuitOpen && now.Sub(c.openedAt) >= circuitCooldown {
			c.state = CircuitHalfOpen
		}
		c.trimWindow(now)
		h := ModelHealth{
			Slug:          slug,
			State:         c.state.String(),
			stateEnum:     c.state,
			RecentErrors:  len(c.errorTimes),
			LastError:     c.lastError,
			TotalRequests: c.totalRequests,
			TotalErrors:   c.totalErrors,
		}
		if !c.lastErrorAt.IsZero() {
			t := c.lastErrorAt
			h.LastErrorAt = &t
		}
		if c.state != CircuitClosed && !c.openedAt.IsZero() {
			t := c.openedAt
			h.OpenedAt = &t
		}
		out = append(out, h)
	}
	return out
}
