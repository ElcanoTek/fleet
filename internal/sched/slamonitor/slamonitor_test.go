package slamonitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeStore implements SLAStore for tests: it returns a fixed task list and
// records every MarkSLABreached call so a test can assert which tasks the
// monitor latched.
type fakeStore struct {
	tasks    []*models.Task
	breached []uuid.UUID
	err      error
}

func (f *fakeStore) GetRunningTasksWithSLA(_ context.Context) ([]*models.Task, error) {
	return f.tasks, f.err
}

func (f *fakeStore) MarkSLABreached(_ context.Context, taskID uuid.UUID) error {
	f.breached = append(f.breached, taskID)
	return nil
}

func minutes(n int) *int { return &n }

func startAt(now time.Time, ago time.Duration) *time.Time {
	t := now.Add(-ago)
	return &t
}

func TestCheck_WarnBelowFail(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID:                      uuid.New(),
				Prompt:                  "daily-report\nsecond line ignored",
				Status:                  models.TaskStatusRunning,
				StartedAt:               startAt(now, 18*time.Minute), // < 22.5 warn (15*1.5); should NOT warn
				ExpectedDurationMinutes: minutes(15),
				SLAWarnMultiplier:       1.5,
				SLAFailMultiplier:       2.0,
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })
	m.Check(context.Background())

	if len(store.breached) != 0 {
		t.Fatalf("expected no breach at 18min on 15min expected, got %v", store.breached)
	}
	if got := metricValue(t, "fleet_task_sla_warn_total", "daily-report"); got != 0 {
		t.Fatalf("warn counter = %v, want 0 (18min < 22.5 warn)", got)
	}
}

func TestCheck_WarnFiresAtThreshold(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID:                      uuid.New(),
				Prompt:                  "daily-report",
				Status:                  models.TaskStatusRunning,
				StartedAt:               startAt(now, 23*time.Minute), // >= 22.5 warn, < 30 fail
				ExpectedDurationMinutes: minutes(15),
				SLAWarnMultiplier:       1.5,
				SLAFailMultiplier:       2.0,
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })
	m.Check(context.Background())

	if len(store.breached) != 0 {
		t.Fatalf("expected no breach at warn-only elapsed, got %v", store.breached)
	}
	if got := metricValue(t, "fleet_task_sla_warn_total", "daily-report"); got != 1 {
		t.Fatalf("warn counter = %v, want 1", got)
	}
}

func TestCheck_FailBreachesAndLatches(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	breachTask := uuid.New()
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID:                      breachTask,
				Prompt:                  "code-review",
				Status:                  models.TaskStatusRunning,
				StartedAt:               startAt(now, 31*time.Minute), // > 40? no. > 30 fail (20*2.0)? yes
				ExpectedDurationMinutes: minutes(20),
				SLAWarnMultiplier:       1.5, // warn at 30
				SLAFailMultiplier:       2.0, // fail at 40 → 31 < 40, so this is actually a WARN
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })
	m.Check(context.Background())

	// 31 min on 20min expected: warn=30, fail=40 → 31 is warn-only, NOT fail.
	if len(store.breached) != 0 {
		t.Fatalf("31min on 20min expected (fail@40) should NOT breach, got %v", store.breached)
	}
}

func TestCheck_FailAtFailThreshold(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	breachTask := uuid.New()
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID:                      breachTask,
				Prompt:                  "code-review",
				Status:                  models.TaskStatusRunning,
				StartedAt:               startAt(now, 41*time.Minute), // >= 40 fail (20*2.0)
				ExpectedDurationMinutes: minutes(20),
				SLAWarnMultiplier:       1.5,
				SLAFailMultiplier:       2.0,
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })
	m.Check(context.Background())

	if len(store.breached) != 1 || store.breached[0] != breachTask {
		t.Fatalf("expected breach of %s, got %v", breachTask, store.breached)
	}
	if got := metricValue(t, "fleet_task_sla_fail_total", "code-review"); got != 1 {
		t.Fatalf("fail counter = %v, want 1", got)
	}
}

func TestCheck_AlreadyBreachedNotRelatched(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	breachTask := uuid.New()
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID:                      breachTask,
				Prompt:                  "code-review",
				Status:                  models.TaskStatusRunning,
				StartedAt:               startAt(now, 41*time.Minute),
				ExpectedDurationMinutes: minutes(20),
				SLAWarnMultiplier:       1.5,
				SLAFailMultiplier:       2.0,
				SLABreached:             true, // already latched
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })
	m.Check(context.Background())

	if len(store.breached) != 0 {
		t.Fatalf("expected no re-latch, got %v", store.breached)
	}
}

func TestCheck_NoSLAIgnored(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID:        uuid.New(),
				Prompt:    "no-sla",
				Status:    models.TaskStatusRunning,
				StartedAt: startAt(now, 3*time.Hour),
				// ExpectedDurationMinutes nil
			},
			{
				ID:                      uuid.New(),
				Prompt:                  "not-started",
				Status:                  models.TaskStatusLeased,
				ExpectedDurationMinutes: minutes(10), // but StartedAt nil
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })
	m.Check(context.Background())

	if len(store.breached) != 0 {
		t.Fatalf("expected no breach for SLA-less / unstarted tasks, got %v", store.breached)
	}
}

func TestCheck_StoreErrorNoOp(t *testing.T) {
	store := &fakeStore{err: sentinelError{}}
	m := New(store)
	m.SetNow(func() time.Time { return time.Now().UTC() })
	m.Check(context.Background()) // must not panic / hang
	if len(store.breached) != 0 {
		t.Fatalf("expected no breach on store error, got %v", store.breached)
	}
}

type sentinelError struct{}

func (sentinelError) Error() string { return "sentinel" }

// TestCheck_DefaultMultipliersApplied confirms a task constructed without
// explicit multipliers (the 0 value) still gets the documented 1.5/2.0
// thresholds via effectiveWarnMul/effectiveFailMul.
func TestCheck_DefaultMultipliersApplied(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID:                      uuid.New(),
				Prompt:                  "defaults",
				Status:                  models.TaskStatusRunning,
				StartedAt:               startAt(now, 31*time.Minute), // warn@30, fail@40 → warn-only
				ExpectedDurationMinutes: minutes(20),
				// SLAWarnMultiplier / SLAFailMultiplier = 0 → defaults apply
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })
	m.Check(context.Background())
	if len(store.breached) != 0 {
		t.Fatalf("31min on 20min expected with defaults (warn@30,fail@40) should NOT breach, got %v", store.breached)
	}
	if got := metricValue(t, "fleet_task_sla_warn_total", "defaults"); got != 1 {
		t.Fatalf("warn counter = %v, want 1", got)
	}
}

func TestTaskName(t *testing.T) {
	cases := []struct {
		prompt string
		want   string
	}{
		{"daily-report\nsecond line", "daily-report"},
		{"short", "short"},
		{strings.Repeat("x", 100), strings.Repeat("x", 64)},
		{"   trimmed   ", "trimmed"},
		{"\nblank-first-line", "untitled"},
		{"", "untitled"},
	}
	for _, c := range cases {
		got := TaskName(&models.Task{Prompt: c.prompt})
		if got != c.want {
			t.Errorf("TaskName(%q) = %q, want %q", c.prompt, got, c.want)
		}
	}
}

// metricValue reads a counter out of the metrics registry's rendered output.
// Coarse but sufficient for the monitor's per-name counter assertions.
func metricValue(t *testing.T, name, taskName string) float64 {
	t.Helper()
	out := metrics.Render()
	label := fmt.Sprintf("task_name=%q", taskName)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		if !strings.Contains(line, label) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse %q: %v", fields[len(fields)-1], err)
		}
		return v
	}
	return 0
}

// TestCheck_WarnFiresOncePerRun: the warning is emitted on the first tick past
// the warn threshold and NOT again on subsequent ticks (it used to log and
// count every 60s for the rest of the run), and a run whose breach is already
// latched does not fall through to the warn branch at all. When the task
// leaves the running set its entry is forgotten, so a fresh attempt warns anew.
func TestCheck_WarnFiresOncePerRun(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	warnTask := uuid.New()
	breached := uuid.New()
	store := &fakeStore{
		tasks: []*models.Task{
			{
				ID: warnTask, Prompt: "slow", Status: models.TaskStatusRunning,
				StartedAt: startAt(now, 31*time.Minute), ExpectedDurationMinutes: minutes(20),
				SLAWarnMultiplier: 1.5, SLAFailMultiplier: 5.0,
			},
			{
				ID: breached, Prompt: "already breached", Status: models.TaskStatusRunning,
				StartedAt: startAt(now, 3*time.Hour), ExpectedDurationMinutes: minutes(20),
				SLAWarnMultiplier: 1.5, SLAFailMultiplier: 2.0, SLABreached: true,
			},
		},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })

	m.Check(context.Background())
	if _, ok := m.warned[warnTask]; !ok {
		t.Fatal("first tick past the warn threshold did not record the warning")
	}
	if _, ok := m.warned[breached]; ok {
		t.Fatal("a latched breach must not be warned about")
	}
	if len(store.breached) != 0 {
		t.Fatalf("nothing should latch here, got %v", store.breached)
	}
	warnedBefore := len(m.warned)
	m.SetNow(func() time.Time { return now.Add(time.Minute) })
	m.Check(context.Background())
	if len(m.warned) != warnedBefore {
		t.Fatalf("second tick changed the warned set (%d → %d); the warning must fire once", warnedBefore, len(m.warned))
	}

	// The run finishes: it drops out of the running set and its entry goes.
	store.tasks = store.tasks[1:]
	m.Check(context.Background())
	if _, ok := m.warned[warnTask]; ok {
		t.Fatal("finished task's warned entry was not pruned")
	}
}

// TestCheck_RetryWarnsAgain covers the gap the id-keyed latch had: a warned
// task fails and its retry starts between two sweeps, so the id never leaves
// the running set and the entry was never pruned — the new attempt's warning
// was swallowed. The latch is keyed by attempt (started_at): the same attempt
// warns once, a new started_at under the same id warns again.
func TestCheck_RetryWarnsAgain(t *testing.T) {
	now := time.Date(2025, 6, 29, 12, 0, 0, 0, time.UTC)
	id := uuid.New()
	firstStart := startAt(now, 31*time.Minute) // warn@30 (20*1.5), fail@100
	store := &fakeStore{
		tasks: []*models.Task{{
			ID: id, Prompt: "retried", Status: models.TaskStatusRunning,
			StartedAt: firstStart, ExpectedDurationMinutes: minutes(20),
			SLAWarnMultiplier: 1.5, SLAFailMultiplier: 5.0,
		}},
	}
	m := New(store)
	m.SetNow(func() time.Time { return now })

	m.Check(context.Background())
	if at, ok := m.warned[id]; !ok || !at.Equal(*firstStart) {
		t.Fatalf("first attempt not latched by its started_at: %v %v", at, ok)
	}

	// Same attempt, next sweep: still latched, nothing changes.
	m.SetNow(func() time.Time { return now.Add(time.Minute) })
	m.Check(context.Background())
	if at := m.warned[id]; !at.Equal(*firstStart) {
		t.Fatalf("second sweep re-latched the same attempt: %v", at)
	}

	// The task failed and retried between sweeps: same id, new started_at, and
	// the retry has itself already run past the warn threshold.
	later := now.Add(2 * time.Hour)
	retryStart := startAt(later, 31*time.Minute)
	store.tasks[0].StartedAt = retryStart
	m.SetNow(func() time.Time { return later })
	m.Check(context.Background())
	if at, ok := m.warned[id]; !ok || !at.Equal(*retryStart) {
		t.Fatalf("retry was not warned about: latch = %v (ok=%v), want %v", at, ok, *retryStart)
	}
	if len(store.breached) != 0 {
		t.Fatalf("nothing should breach here, got %v", store.breached)
	}
}
