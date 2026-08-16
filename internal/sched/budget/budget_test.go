// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package budget

// Unit tests for the Enforcer's decision logic against a fake Store: window
// math, dollar AND token bounds, the fail-safe clamp against the live global
// ceilings, once-per-window soft alerting, and the no-budget fast path. The
// end-to-end behavior over a real database (every create path, restart
// semantics) is covered by the DB-backed tests in internal/sched/handlers.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// fakeStore implements Store in memory. Markers behave like the SQL claim:
// conditional on the stored window differing.
type fakeStore struct {
	budgets    []models.Budget
	taskUsage  []models.UsageBucket
	usageCalls int
	markers    map[uuid.UUID]time.Time
}

func (f *fakeStore) BudgetsFor(_ context.Context, user, key string) ([]models.Budget, error) {
	var out []models.Budget
	for _, b := range f.budgets {
		switch {
		case b.Scope == models.BudgetScopeUser && user != "" && b.PrincipalID == user,
			b.Scope == models.BudgetScopeKey && key != "" && b.PrincipalID == key:
			bb := b
			if ws, ok := f.markers[b.ID]; ok {
				w := ws
				bb.SoftAlertWindowStart = &w
			}
			out = append(out, bb)
		}
	}
	return out, nil
}

func (f *fakeStore) ListBudgets(_ context.Context) ([]models.Budget, error) {
	return f.budgets, nil
}

func (f *fakeStore) TaskUsage(_ context.Context, _, _ time.Time, _ string) ([]models.UsageBucket, error) {
	f.usageCalls++
	return f.taskUsage, nil
}

func (f *fakeStore) MarkBudgetSoftAlert(_ context.Context, id uuid.UUID, ws time.Time) (bool, error) {
	if f.markers == nil {
		f.markers = map[uuid.UUID]time.Time{}
	}
	if prev, ok := f.markers[id]; ok && prev.Equal(ws) {
		return false, nil
	}
	f.markers[id] = ws
	return true, nil
}

type fakeNotifier struct{ events []notify.Event }

func (f *fakeNotifier) Notify(_ context.Context, ev notify.Event) error {
	f.events = append(f.events, ev)
	return nil
}

// taskSpend builds a task-side usage bucket the way db.TaskUsage does.
func taskSpend(key string, usd float64, tokens int64) models.UsageBucket {
	return models.UsageBucket{Key: key, CostUSD: usd, TaskCostUSD: usd, PromptTokens: tokens}
}

func TestWindowStart(t *testing.T) {
	// 2026-07-02 is a Thursday.
	now := time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC)
	cases := []struct {
		window string
		start  time.Time
		end    time.Time
	}{
		{models.BudgetWindowDay, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)},
		{models.BudgetWindowWeek, time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)},
		{models.BudgetWindowMonth, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		ws := windowStart(now, c.window)
		if !ws.Equal(c.start) {
			t.Errorf("windowStart(%s) = %v, want %v", c.window, ws, c.start)
		}
		if we := windowEnd(ws, c.window); !we.Equal(c.end) {
			t.Errorf("windowEnd(%s) = %v, want %v", c.window, we, c.end)
		}
	}
	// A Monday must be its own week start (boundary), and Sunday the last day.
	monday := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	if ws := windowStart(monday, models.BudgetWindowWeek); !ws.Equal(monday) {
		t.Errorf("Monday week start = %v, want %v", ws, monday)
	}
	sunday := time.Date(2026, 7, 5, 23, 59, 0, 0, time.UTC)
	if ws := windowStart(sunday, models.BudgetWindowWeek); !ws.Equal(monday) {
		t.Errorf("Sunday week start = %v, want %v", ws, monday)
	}
}

func TestCheckCreate_NoBudget_NoAggregation(t *testing.T) {
	store := &fakeStore{}
	e := New(Config{Store: store})
	if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice@example.com", Key: "key-1"}); err != nil {
		t.Fatalf("CheckCreate with no budgets: %v", err)
	}
	if store.usageCalls != 0 {
		t.Errorf("no-budget path must not aggregate usage (got %d calls)", store.usageCalls)
	}
	// No principals at all (admin-key create): also a nil no-op.
	if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{}); err != nil {
		t.Fatalf("CheckCreate with no principals: %v", err)
	}
}

func TestCheckCreate_HardBounds(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	mkEnforcer := func(b models.Budget, spend models.UsageBucket) (*Enforcer, *fakeStore) {
		store := &fakeStore{budgets: []models.Budget{b}, taskUsage: []models.UsageBucket{spend}}
		return New(Config{Store: store, Now: func() time.Time { return now }, SyncNotify: true}), store
	}

	t.Run("under the hard bound admits", func(t *testing.T) {
		e, _ := mkEnforcer(
			models.Budget{ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice", Window: models.BudgetWindowDay, HardUSD: f64(10)},
			taskSpend("alice", 9.99, 0))
		if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"}); err != nil {
			t.Fatalf("want admit, got %v", err)
		}
	})

	t.Run("dollar hard bound refuses", func(t *testing.T) {
		e, _ := mkEnforcer(
			models.Budget{ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice", Window: models.BudgetWindowDay, HardUSD: f64(10)},
			taskSpend("alice", 10, 0))
		err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"})
		var exceeded *ExceededError
		if !errors.As(err, &exceeded) {
			t.Fatalf("want ExceededError, got %v", err)
		}
		if exceeded.LimitUSD == nil || *exceeded.LimitUSD != 10 || exceeded.LimitTokens != nil {
			t.Errorf("wrong refusing limit: %+v", exceeded)
		}
		if want := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC); !exceeded.WindowEnd.Equal(want) {
			t.Errorf("WindowEnd = %v, want %v", exceeded.WindowEnd, want)
		}
	})

	t.Run("token hard bound refuses even at zero dollars", func(t *testing.T) {
		// Native-provider honest scope (#289): $0 cost, tokens still bound.
		e, _ := mkEnforcer(
			models.Budget{ID: uuid.New(), Scope: models.BudgetScopeKey, PrincipalID: "key-1", Window: models.BudgetWindowMonth, HardTokens: i64(5000)},
			taskSpend("key-1", 0, 5000))
		err := e.CheckCreate(context.Background(), models.BudgetPrincipals{Key: "key-1"})
		var exceeded *ExceededError
		if !errors.As(err, &exceeded) {
			t.Fatalf("want ExceededError, got %v", err)
		}
		if exceeded.LimitTokens == nil || *exceeded.LimitTokens != 5000 {
			t.Errorf("wrong refusing limit: %+v", exceeded)
		}
	})

	t.Run("other principals' spend does not count", func(t *testing.T) {
		e, _ := mkEnforcer(
			models.Budget{ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice", Window: models.BudgetWindowDay, HardUSD: f64(10)},
			taskSpend("bob", 100, 0))
		if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"}); err != nil {
			t.Fatalf("want admit (bob's spend is not alice's), got %v", err)
		}
	})
}

func TestCheckCreate_ChatSpendCounts(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		budgets:   []models.Budget{{ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice", Window: models.BudgetWindowDay, HardUSD: f64(10)}},
		taskUsage: []models.UsageBucket{taskSpend("alice", 6, 0)},
	}
	chat := func(_ context.Context, _, _ time.Time, _ string) ([]models.UsageBucket, error) {
		return []models.UsageBucket{{Key: "alice", ChatCostUSD: 4}}, nil
	}
	e := New(Config{Store: store, ChatUsage: chat, Now: func() time.Time { return now }})
	err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"})
	var exceeded *ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("want ExceededError (6 task + 4 chat >= 10), got %v", err)
	}
	if exceeded.SpendUSD != 10 {
		t.Errorf("SpendUSD = %v, want 10 (both meters summed)", exceeded.SpendUSD)
	}
}

func TestCheckCreate_GlobalCeilingClamp(t *testing.T) {
	// Fail-safe composition (#286): a budget's hard bound can never exceed the
	// LIVE global ceilings — min(budget, ceiling) refuses even though the
	// budget row alone would admit.
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	liveCost, liveTokens := 100.0, 0
	store := &fakeStore{
		budgets:   []models.Budget{{ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice", Window: models.BudgetWindowMonth, HardUSD: f64(1000)}},
		taskUsage: []models.UsageBucket{taskSpend("alice", 150, 0)},
	}
	e := New(Config{
		Store:    store,
		Now:      func() time.Time { return now },
		Ceilings: func() (float64, int) { return liveCost, liveTokens },
	})
	err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"})
	var exceeded *ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("want ExceededError (spend 150 >= min(1000, live 100)), got %v", err)
	}
	if exceeded.LimitUSD == nil || *exceeded.LimitUSD != 100 {
		t.Errorf("effective limit = %v, want the clamped 100", exceeded.LimitUSD)
	}

	// The ceilings are read LIVE: a hot-reload raising the global above the
	// budget puts the budget's own bound back in charge — still refusing here
	// (150 >= 1000 is false → admit).
	liveCost = 5000
	if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"}); err != nil {
		t.Fatalf("after live raise, budget bound (1000) governs and 150 < 1000 must admit: %v", err)
	}

	// A disabled ceiling (0) never clamps.
	liveCost = 0
	if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"}); err != nil {
		t.Fatalf("ceiling 0 = disabled, no clamp: %v", err)
	}
}

func TestCheckCreate_SoftAlertOncePerWindow(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	notifier := &fakeNotifier{}
	store := &fakeStore{
		budgets:   []models.Budget{{ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice@example.com", Window: models.BudgetWindowDay, SoftUSD: f64(5), HardUSD: f64(50)}},
		taskUsage: []models.UsageBucket{taskSpend("alice@example.com", 6, 100)},
	}
	e := New(Config{Store: store, Notifier: notifier, SyncNotify: true, Now: func() time.Time { return now }})

	// First create over the soft bound: admitted, exactly one alert.
	if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice@example.com"}); err != nil {
		t.Fatalf("soft crossing must still admit: %v", err)
	}
	if len(notifier.events) != 1 {
		t.Fatalf("want exactly 1 alert after first crossing, got %d", len(notifier.events))
	}
	ev := notifier.events[0]
	if ev.Status != notify.StatusProgress {
		t.Errorf("alert status = %q, want progress", ev.Status)
	}
	if ev.Audience != "alice@example.com" {
		t.Errorf("user-scope alert audience = %q, want the principal email", ev.Audience)
	}

	// Repeat creates in the same window: no duplicate spam.
	for i := 0; i < 3; i++ {
		if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice@example.com"}); err != nil {
			t.Fatalf("repeat create %d: %v", i, err)
		}
	}
	if len(notifier.events) != 1 {
		t.Fatalf("want still 1 alert after repeats, got %d", len(notifier.events))
	}

	// Window rollover: the next day's crossing alerts again (and the refusal
	// window resets with it).
	now = now.AddDate(0, 0, 1)
	if err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice@example.com"}); err != nil {
		t.Fatalf("next-window create: %v", err)
	}
	if len(notifier.events) != 2 {
		t.Fatalf("want 2 alerts after window rollover, got %d", len(notifier.events))
	}
}

func TestCheckCreate_HardRefusalStillFiresSoftAlert(t *testing.T) {
	// If the FIRST observation of the window is already past both bounds, the
	// operator still gets the one soft alert alongside the refusal.
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	notifier := &fakeNotifier{}
	store := &fakeStore{
		budgets:   []models.Budget{{ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice", Window: models.BudgetWindowDay, SoftUSD: f64(5), HardUSD: f64(10)}},
		taskUsage: []models.UsageBucket{taskSpend("alice", 12, 0)},
	}
	e := New(Config{Store: store, Notifier: notifier, SyncNotify: true, Now: func() time.Time { return now }})
	err := e.CheckCreate(context.Background(), models.BudgetPrincipals{User: "alice"})
	var exceeded *ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("want ExceededError, got %v", err)
	}
	if len(notifier.events) != 1 {
		t.Errorf("want the soft alert to fire alongside the refusal, got %d events", len(notifier.events))
	}
}

func TestStatuses(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	ws := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{
		budgets: []models.Budget{{
			ID: uuid.New(), Scope: models.BudgetScopeUser, PrincipalID: "alice",
			Window: models.BudgetWindowMonth, SoftUSD: f64(5), HardUSD: f64(500),
			SoftAlertWindowStart: &ws,
		}},
		taskUsage: []models.UsageBucket{taskSpend("alice", 7, 1234)},
	}
	e := New(Config{Store: store, Now: func() time.Time { return now }, Ceilings: func() (float64, int) { return 100, 0 }})
	statuses, err := e.Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("want 1 status, got %d", len(statuses))
	}
	s := statuses[0]
	if !s.WindowStart.Equal(ws) || !s.WindowEnd.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window = [%v, %v)", s.WindowStart, s.WindowEnd)
	}
	if s.SpendUSD != 7 || s.SpendTokens != 1234 {
		t.Errorf("spend = $%v / %d tokens, want $7 / 1234", s.SpendUSD, s.SpendTokens)
	}
	if s.EffectiveHardUSD == nil || *s.EffectiveHardUSD != 100 {
		t.Errorf("effective hard = %v, want the clamped 100", s.EffectiveHardUSD)
	}
	if !s.SoftAlerted {
		t.Error("SoftAlerted should be true for the current window's marker")
	}
}

func TestExceededErrorMessage(t *testing.T) {
	err := &ExceededError{
		Budget:   models.Budget{Scope: models.BudgetScopeUser, PrincipalID: "alice@example.com", Window: models.BudgetWindowDay},
		SpendUSD: 12.5, SpendTokens: 9000,
		LimitUSD:  f64(10),
		WindowEnd: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
	}
	msg := err.Error()
	for _, want := range []string{"budget exceeded", "alice@example.com", "day window", "$10.00", "2026-07-03T00:00:00Z"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}
