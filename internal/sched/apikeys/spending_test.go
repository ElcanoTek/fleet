package apikeys

import (
	"path/filepath"
	"testing"
	"time"
)

func newSpendingManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	m, err := NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, path
}

func TestSpendingCaps_AccumulateAndCheck(t *testing.T) {
	m, _ := newSpendingManager(t)
	key, _, err := m.CreateKey("ci", nil, nil, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	dailyCap := 10.0
	if err := m.SetBudgets(key.KeyID, &dailyCap, nil); err != nil {
		t.Fatalf("SetBudgets: %v", err)
	}

	if err := m.CheckBudget(key.KeyID); err != nil {
		t.Fatalf("under budget should pass: %v", err)
	}
	m.AccumulateCost(key.KeyID, 7)
	if err := m.CheckBudget(key.KeyID); err != nil {
		t.Fatalf("7 < 10 should pass: %v", err)
	}
	m.AccumulateCost(key.KeyID, 5) // 12 >= 10
	if err := m.CheckBudget(key.KeyID); err == nil {
		t.Fatal("12 >= 10 daily cap should be rejected")
	}

	snap, ok := m.SpendingSnapshot(key.KeyID)
	if !ok {
		t.Fatal("snapshot not found")
	}
	if snap.CostTodayUSD != 12 {
		t.Errorf("snapshot CostTodayUSD = %v, want 12", snap.CostTodayUSD)
	}
	if snap.MaxCostPerDayUSD == nil || *snap.MaxCostPerDayUSD != 10 {
		t.Errorf("snapshot cap = %v, want 10", snap.MaxCostPerDayUSD)
	}

	if err := m.ResetSpending(key.KeyID); err != nil {
		t.Fatalf("ResetSpending: %v", err)
	}
	if err := m.CheckBudget(key.KeyID); err != nil {
		t.Fatalf("after reset should pass: %v", err)
	}
}

func TestSpendingCaps_NoCapAlwaysPasses(t *testing.T) {
	m, _ := newSpendingManager(t)
	key, _, _ := m.CreateKey("ci", nil, nil, nil, 0, nil, "")
	m.AccumulateCost(key.KeyID, 9999)
	if err := m.CheckBudget(key.KeyID); err != nil {
		t.Errorf("no cap configured should always pass, got: %v", err)
	}
}

func TestSpendingCaps_Persist(t *testing.T) {
	m, path := newSpendingManager(t)
	key, _, _ := m.CreateKey("ci", nil, nil, nil, 0, nil, "")
	m.AccumulateCost(key.KeyID, 3.5)

	// Reload from disk: the accumulated spend survives.
	m2, err := NewManager(path, filepath.Join(filepath.Dir(path), "audit.jsonl"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	snap, ok := m2.SpendingSnapshot(key.KeyID)
	if !ok || snap.CostTodayUSD != 3.5 {
		t.Errorf("reloaded CostTodayUSD = %v (ok=%v), want 3.5", snap.CostTodayUSD, ok)
	}
}

func TestSpendingCaps_LazyDailyReset(t *testing.T) {
	m, _ := newSpendingManager(t)
	key, _, _ := m.CreateKey("ci", nil, nil, nil, 0, nil, "")
	m.AccumulateCost(key.KeyID, 5)

	// Backdate the daily window to two days ago; the next access should reset it.
	m.mu.Lock()
	m.keys[key.KeyID].CostDayResetAt = time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour)
	m.mu.Unlock()

	snap, _ := m.SpendingSnapshot(key.KeyID)
	if snap.CostTodayUSD != 0 {
		t.Errorf("daily counter not reset across the day boundary: %v", snap.CostTodayUSD)
	}
}
