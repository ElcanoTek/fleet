package db

// DB-backed tests for the budgets table (#601 part 2, migration 052): CRUD,
// the upsert-on-(scope,principal,window) contract, and the once-per-window
// soft-alert claim that backs "exactly one alert per window crossing".
// Gated on DATABASE_URL like the rest of the package.

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func cleanBudgets(t *testing.T, db *Database) {
	t.Helper()
	if _, err := db.conn.ExecContext(context.Background(), "DELETE FROM budgets"); err != nil {
		t.Fatalf("clean budgets: %v", err)
	}
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestBudgetCRUDAndUpsert(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanBudgets(t, db)
	ctx := context.Background()

	b, err := db.UpsertBudget(ctx, models.BudgetCreate{
		Scope: models.BudgetScopeUser, PrincipalID: "alice@example.com",
		Window: models.BudgetWindowDay, SoftUSD: f64(5), HardUSD: f64(10), HardTokens: i64(1000),
	})
	if err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}
	if b.ID.String() == "" || b.SoftUSD == nil || *b.SoftUSD != 5 || b.HardTokens == nil || *b.HardTokens != 1000 {
		t.Fatalf("unexpected persisted budget: %+v", b)
	}
	if b.SoftTokens != nil {
		t.Fatalf("soft_tokens should be unset, got %v", *b.SoftTokens)
	}

	// Mark the soft alert, then upsert the same (scope, principal, window):
	// the row is REPLACED in place (same natural key), bounds updated, and the
	// alert marker reset so the edited budget re-arms.
	ws := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if fired, err := db.MarkBudgetSoftAlert(ctx, b.ID, ws); err != nil || !fired {
		t.Fatalf("MarkBudgetSoftAlert: fired=%v err=%v", fired, err)
	}
	b2, err := db.UpsertBudget(ctx, models.BudgetCreate{
		Scope: models.BudgetScopeUser, PrincipalID: "alice@example.com",
		Window: models.BudgetWindowDay, HardUSD: f64(20),
	})
	if err != nil {
		t.Fatalf("UpsertBudget (replace): %v", err)
	}
	if b2.ID != b.ID {
		t.Errorf("upsert must keep the row identity: got %s want %s", b2.ID, b.ID)
	}
	if b2.SoftUSD != nil || b2.HardUSD == nil || *b2.HardUSD != 20 {
		t.Errorf("upsert must replace bounds: %+v", b2)
	}
	if b2.SoftAlertWindowStart != nil {
		t.Errorf("upsert must reset the soft-alert marker, got %v", b2.SoftAlertWindowStart)
	}

	// A second window kind for the same principal is a distinct row.
	if _, err := db.UpsertBudget(ctx, models.BudgetCreate{
		Scope: models.BudgetScopeUser, PrincipalID: "alice@example.com",
		Window: models.BudgetWindowMonth, HardUSD: f64(100),
	}); err != nil {
		t.Fatalf("UpsertBudget (month): %v", err)
	}
	all, err := db.ListBudgets(ctx)
	if err != nil {
		t.Fatalf("ListBudgets: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 budgets, got %d", len(all))
	}

	// BudgetsFor matches only the named principals; empty args match nothing.
	got, err := db.BudgetsFor(ctx, "alice@example.com", "")
	if err != nil {
		t.Fatalf("BudgetsFor: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("BudgetsFor(alice) = %d budgets, want 2", len(got))
	}
	if got, _ := db.BudgetsFor(ctx, "bob@example.com", ""); len(got) != 0 {
		t.Errorf("BudgetsFor(bob) = %d budgets, want 0", len(got))
	}
	if got, _ := db.BudgetsFor(ctx, "", ""); len(got) != 0 {
		t.Errorf("BudgetsFor(empty) = %d budgets, want 0 (empty principals must never match)", len(got))
	}

	deleted, err := db.DeleteBudget(ctx, b2.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteBudget: deleted=%v err=%v", deleted, err)
	}
	if deleted, _ := db.DeleteBudget(ctx, b2.ID); deleted {
		t.Error("DeleteBudget must report false for a missing row")
	}
}

func TestMarkBudgetSoftAlert_OncePerWindow(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	cleanBudgets(t, db)
	ctx := context.Background()

	b, err := db.UpsertBudget(ctx, models.BudgetCreate{
		Scope: models.BudgetScopeKey, PrincipalID: "key-1",
		Window: models.BudgetWindowWeek, SoftUSD: f64(1),
	})
	if err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}

	week1 := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC) // a Monday
	if fired, err := db.MarkBudgetSoftAlert(ctx, b.ID, week1); err != nil || !fired {
		t.Fatalf("first claim: fired=%v err=%v (want true)", fired, err)
	}
	// Same window: the claim must lose — this is the dedup that survives
	// restarts (state is the DB row, not process memory).
	if fired, err := db.MarkBudgetSoftAlert(ctx, b.ID, week1); err != nil || fired {
		t.Fatalf("repeat claim: fired=%v err=%v (want false)", fired, err)
	}
	// Window rollover: a NEW window start wins again.
	week2 := week1.AddDate(0, 0, 7)
	if fired, err := db.MarkBudgetSoftAlert(ctx, b.ID, week2); err != nil || !fired {
		t.Fatalf("next-window claim: fired=%v err=%v (want true)", fired, err)
	}

	got, err := db.ListBudgets(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListBudgets: %v (%d rows)", err, len(got))
	}
	if got[0].SoftAlertWindowStart == nil || !got[0].SoftAlertWindowStart.Equal(week2) {
		t.Errorf("marker = %v, want %v", got[0].SoftAlertWindowStart, week2)
	}
	if got[0].SoftAlertAt == nil {
		t.Error("soft_alert_at should be set after a claim")
	}
}
