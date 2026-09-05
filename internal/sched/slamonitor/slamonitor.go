// Package slamonitor runs the SLA monitoring goroutine (#274): a background
// sweep that, every minute, inspects in-flight tasks carrying an
// expected_duration_minutes and emits a warn / fail signal when the elapsed
// wall-clock crosses the corresponding threshold.
//
// The monitor is deliberately a READ + LOG + COUNTER loop — it never cancels a
// task (the per-turn agent-core iteration ceiling is the kill switch; SLA is
// an observability/alerting surface). The one write it performs is latching
// sla_breached=true so the SLA report and UI can surface the breach without
// recomputing thresholds. Failures are logged and counted; they never kill the
// loop or the process.
package slamonitor

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// SLAStore is the narrow storage seam the monitor needs. It is satisfied by
// *storage.Storage in production and a fake in tests, so the monitor is
// decoupled from the full storage surface (and from a live database).
type SLAStore interface {
	GetRunningTasksWithSLA(ctx context.Context) ([]*models.Task, error)
	MarkSLABreached(ctx context.Context, taskID uuid.UUID) error
}

// nowFunc returns the current time. time.Now by default; overridable in tests
// so the monitor's threshold math can be exercised without real sleeps.
type nowFunc func() time.Time

// SLAMonitor runs the periodic SLA check (#274). It is started once from
// cmd/fleet alongside the scheduler ticker and runs until its Stop channel is
// closed or the context is cancelled.
type SLAMonitor struct {
	store SLAStore
	now   nowFunc
	stop  chan struct{}

	// warned remembers which running tasks have already had their sla_warn
	// emitted, so the warning (log line + counter) fires ONCE per run rather
	// than on every 60s tick for the rest of the run. Pruned each sweep to the
	// tasks still running, so a finished task's entry (and a retry's — a fresh
	// attempt has a fresh started_at and deserves a fresh warning) is dropped.
	// Guarded by mu because Check is also driven directly by tests.
	mu     sync.Mutex
	warned map[uuid.UUID]struct{}
}

// New constructs an SLAMonitor backed by store. The store MUST implement
// GetRunningTasksWithSLA + MarkSLABreached (storage.Storage does).
func New(store SLAStore) *SLAMonitor {
	return &SLAMonitor{
		store:  store,
		now:    func() time.Time { return time.Now().UTC() },
		stop:   make(chan struct{}),
		warned: make(map[uuid.UUID]struct{}),
	}
}

// Start launches the monitor goroutine. It returns immediately; the loop runs
// until ctx is cancelled or Stop is called. Failures inside a tick are
// recovered (safe.Recover) so a panic in the check never kills the process —
// matching the scheduler's own crash-safety posture.
func (m *SLAMonitor) Start(ctx context.Context) {
	log.Println("Starting SLA monitor...")
	go m.run(ctx)
}

// Stop signals the monitor loop to exit. Safe to call once; a second call panics
// on the double close (mirroring scheduler.Stop's contract).
func (m *SLAMonitor) Stop() { close(m.stop) }

// SetNow installs an alternate now-source (tests only). Must be called before
// Start. The default returns time.Now().UTC().
func (m *SLAMonitor) SetNow(fn nowFunc) {
	if fn != nil {
		m.now = fn
	}
}

func (m *SLAMonitor) run(ctx context.Context) {
	defer safe.Recover("slamonitor.run", nil)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			func() {
				defer safe.Recover("slamonitor.tick", nil)
				m.Check(ctx)
			}()
		}
	}
}

// Check performs one SLA sweep. It is exported (PascalCase) so a test can drive
// a single pass without waiting on the 60s ticker.
func (m *SLAMonitor) Check(ctx context.Context) {
	tasks, err := m.store.GetRunningTasksWithSLA(ctx)
	if err != nil {
		log.Printf("sla-monitor: query failed: %v", err)
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	running := make(map[uuid.UUID]struct{}, len(tasks))
	for _, t := range tasks {
		running[t.ID] = struct{}{}
		if t.StartedAt == nil || t.ExpectedDurationMinutes == nil {
			continue
		}
		elapsed := now.Sub(*t.StartedAt)
		expected := time.Duration(*t.ExpectedDurationMinutes) * time.Minute
		warnAt := time.Duration(float64(expected) * effectiveWarnMul(t.SLAWarnMultiplier))
		failAt := time.Duration(float64(expected) * effectiveFailMul(t.SLAFailMultiplier))

		switch {
		case t.SLABreached:
			// Already latched: the breach was reported when it happened, and a
			// breached run has nothing further to warn about. (Before, a latched
			// task fell through to the warn branch and logged sla-warn every
			// tick until it finished.)
		case elapsed >= failAt:
			log.Printf("sla-breach: task_id=%s task_name=%q elapsed_min=%.1f expected_min=%d",
				t.ID, TaskName(t), elapsed.Minutes(), *t.ExpectedDurationMinutes)
			metrics.RecordSLAFail(TaskName(t))
			if err := m.store.MarkSLABreached(ctx, t.ID); err != nil {
				log.Printf("sla-monitor: mark-breached failed for %s: %v", t.ID, err)
			}
		case elapsed >= warnAt:
			if _, done := m.warned[t.ID]; done {
				break
			}
			m.warned[t.ID] = struct{}{}
			log.Printf("sla-warn: task_id=%s task_name=%q elapsed_min=%.1f expected_min=%d",
				t.ID, TaskName(t), elapsed.Minutes(), *t.ExpectedDurationMinutes)
			metrics.RecordSLAWarn(TaskName(t))
		}
	}
	for id := range m.warned {
		if _, still := running[id]; !still {
			delete(m.warned, id)
		}
	}
}

// effectiveWarnMul / effectiveFailMul resolve a multiplier to the default when
// it is the zero value, mirroring NewTask's normalization so a row that bypassed
// NewTask (or a default-column read that scanned 0) still gets the documented
// threshold. A positive multiplier passes through verbatim.
func effectiveWarnMul(v float64) float64 {
	if v <= 0 {
		return models.DefaultSLAWarnMultiplier
	}
	return v
}
func effectiveFailMul(v float64) float64 {
	if v <= 0 {
		return models.DefaultSLAFailMultiplier
	}
	return v
}

// TaskName derives the bounded task-name label for the SLA metrics + report
// (#274). A titled task uses its title — that is the operator's own name for the
// job, it is carried onto every occurrence, and it is already short. Untitled
// tasks fall back to the prompt, the closest stable grouping key — but a raw
// prompt is unbounded free-form text (the cardinality anti-pattern
// RecordDeadLetterQueued calls out). Either way, truncate to the first non-empty
// line and cap at 64 chars so recurring tasks collapse to one series while
// still being recognizable.
func TaskName(t *models.Task) string {
	p := t.Prompt
	if title := strings.TrimSpace(t.Title); title != "" {
		p = title
	}
	if i := strings.IndexByte(p, '\n'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSpace(p)
	if len(p) > 64 {
		p = p[:64]
	}
	if p == "" {
		return "untitled"
	}
	return p
}
