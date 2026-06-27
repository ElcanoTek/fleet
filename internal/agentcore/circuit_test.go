package agentcore

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestRegistry returns a registry with a controllable clock.
func newTestRegistry() (*ProviderHealthRegistry, *atomic.Int64) {
	var nowNanos atomic.Int64
	nowNanos.Store(time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC).UnixNano())
	r := &ProviderHealthRegistry{
		models: map[string]*modelCircuit{},
		now:    func() time.Time { return time.Unix(0, nowNanos.Load()) },
	}
	return r, &nowNanos
}

func advance(clock *atomic.Int64, d time.Duration) { clock.Add(int64(d)) }

func TestCircuit_OpensAfterThreshold(t *testing.T) {
	r, _ := newTestRegistry()
	const slug = "anthropic/claude"
	for i := 0; i < circuitOpenThreshold-1; i++ {
		if st := r.RecordError(slug, "503"); st != CircuitClosed {
			t.Fatalf("error %d: state=%v, want closed", i+1, st)
		}
	}
	if st := r.RecordError(slug, "503"); st != CircuitOpen {
		t.Fatalf("threshold error: state=%v, want open", st)
	}
	if st := r.State(slug); st != CircuitOpen {
		t.Errorf("State() = %v, want open", st)
	}
}

func TestCircuit_SuccessClosesAndResets(t *testing.T) {
	r, _ := newTestRegistry()
	const slug = "m"
	for i := 0; i < circuitOpenThreshold; i++ {
		r.RecordError(slug, "503")
	}
	if r.State(slug) != CircuitOpen {
		t.Fatal("precondition: circuit should be open")
	}
	r.RecordSuccess(slug)
	if st := r.State(slug); st != CircuitClosed {
		t.Errorf("after success: state=%v, want closed", st)
	}
	// Window was cleared: it takes a fresh full threshold to re-open.
	for i := 0; i < circuitOpenThreshold-1; i++ {
		r.RecordError(slug, "503")
	}
	if st := r.State(slug); st != CircuitClosed {
		t.Errorf("error window not reset after success: state=%v, want closed", st)
	}
}

func TestCircuit_CooldownToHalfOpen(t *testing.T) {
	r, clock := newTestRegistry()
	const slug = "m"
	for i := 0; i < circuitOpenThreshold; i++ {
		r.RecordError(slug, "503")
	}
	if r.State(slug) != CircuitOpen {
		t.Fatal("should be open")
	}
	advance(clock, circuitCooldown+time.Second)
	if st := r.State(slug); st != CircuitHalfOpen {
		t.Errorf("after cooldown: state=%v, want half-open", st)
	}
}

func TestCircuit_HalfOpenProbeSuccessCloses(t *testing.T) {
	r, clock := newTestRegistry()
	const slug = "m"
	for i := 0; i < circuitOpenThreshold; i++ {
		r.RecordError(slug, "503")
	}
	advance(clock, circuitCooldown+time.Second)
	_ = r.State(slug) // transition to half-open
	r.RecordSuccess(slug)
	if st := r.State(slug); st != CircuitClosed {
		t.Errorf("probe success: state=%v, want closed", st)
	}
}

func TestCircuit_HalfOpenProbeFailReopens(t *testing.T) {
	r, clock := newTestRegistry()
	const slug = "m"
	for i := 0; i < circuitOpenThreshold; i++ {
		r.RecordError(slug, "503")
	}
	advance(clock, circuitCooldown+time.Second)
	if r.State(slug) != CircuitHalfOpen {
		t.Fatal("should be half-open")
	}
	if st := r.RecordError(slug, "503 again"); st != CircuitOpen {
		t.Errorf("probe failure: state=%v, want open", st)
	}
}

func TestCircuit_WindowEviction(t *testing.T) {
	r, clock := newTestRegistry()
	const slug = "m"
	// Spread errors so old ones age out of the window before the threshold is met.
	for i := 0; i < circuitOpenThreshold*2; i++ {
		r.RecordError(slug, "503")
		advance(clock, circuitWindow/2) // each step ages half the window
	}
	if st := r.State(slug); st == CircuitOpen {
		t.Errorf("circuit opened despite errors spread beyond the window: state=%v", st)
	}
}

func TestCircuit_NilRegistry(t *testing.T) {
	var r *ProviderHealthRegistry
	if r.State("m") != CircuitClosed {
		t.Error("nil registry State should be closed")
	}
	if r.RecordError("m", "x") != CircuitClosed {
		t.Error("nil registry RecordError should be closed")
	}
	r.RecordSuccess("m") // must not panic
	if r.Snapshot() != nil {
		t.Error("nil registry Snapshot should be nil")
	}
}

func TestCircuit_Snapshot(t *testing.T) {
	r, _ := newTestRegistry()
	r.RecordSuccess("good")
	for i := 0; i < circuitOpenThreshold; i++ {
		r.RecordError("bad", "503 Service Unavailable")
	}
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len=%d, want 2", len(snap))
	}
	bySlug := map[string]ModelHealth{}
	for _, h := range snap {
		bySlug[h.Slug] = h
	}
	if bySlug["good"].State != "closed" {
		t.Errorf("good state=%q, want closed", bySlug["good"].State)
	}
	if bySlug["bad"].State != "open" {
		t.Errorf("bad state=%q, want open", bySlug["bad"].State)
	}
	if bySlug["bad"].LastError != "503 Service Unavailable" {
		t.Errorf("bad last_error=%q", bySlug["bad"].LastError)
	}
	if bySlug["bad"].OpenedAt == nil {
		t.Error("bad opened_at should be set")
	}
}

func TestCircuit_ConcurrentAccess(t *testing.T) {
	r := NewProviderHealthRegistry()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				switch i % 3 {
				case 0:
					r.RecordError("m", "503")
				case 1:
					r.RecordSuccess("m")
				default:
					_ = r.State("m")
					_ = r.Snapshot()
				}
			}
		}()
	}
	wg.Wait()
	// Sanity: the slug was tracked (also gives the race detector something to
	// read concurrently with the writers above having finished).
	if len(r.Snapshot()) == 0 {
		t.Error("expected the model to be tracked after concurrent access")
	}
}
