package db

import (
	"strings"
	"testing"
)

// TestAvailableMigrations checks the embedded up-migration enumerator: strictly
// ascending, positive versions, and names with the .up.sql suffix stripped. Pure
// (no DB), so it always runs.
func TestAvailableMigrations(t *testing.T) {
	avail, err := availableMigrations()
	if err != nil {
		t.Fatalf("availableMigrations: %v", err)
	}
	if len(avail) == 0 {
		t.Fatal("expected embedded up-migrations, got none")
	}
	for i, m := range avail {
		if m.Version <= 0 {
			t.Errorf("non-positive version %d for %q", m.Version, m.Name)
		}
		if strings.Contains(m.Name, ".") {
			t.Errorf("name %q should have the .up.sql suffix stripped", m.Name)
		}
		if i > 0 && avail[i-1].Version >= m.Version {
			t.Errorf("availableMigrations not strictly ascending at index %d (%d then %d)", i, avail[i-1].Version, m.Version)
		}
	}
}

// TestMigrationStatus checks the applied-vs-pending report against a freshly
// migrated sched DB. Skips without DATABASE_URL (integration test).
func TestMigrationStatus(t *testing.T) {
	db := setupTestDB(t) // Init runs every migration

	rep, err := MigrationStatus(db.Conn())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if rep.DB != "sched" || rep.Runner != "golang-migrate" || rep.MigrationTable != "schema_migrations" {
		t.Fatalf("unexpected metadata: %+v", rep)
	}
	if rep.Dirty {
		t.Errorf("a freshly migrated DB should not be dirty")
	}
	avail, err := availableMigrations()
	if err != nil {
		t.Fatalf("availableMigrations: %v", err)
	}
	if rep.CurrentVersion == nil {
		t.Fatal("current version should be set after migrations run")
	}
	if latest := avail[len(avail)-1].Version; *rep.CurrentVersion != latest {
		t.Errorf("current version = %d, want the latest embedded version %d", *rep.CurrentVersion, latest)
	}
	if len(rep.Pending) != 0 {
		t.Errorf("a fully-migrated DB should report 0 pending, got %d: %+v", len(rep.Pending), rep.Pending)
	}
	if len(rep.Applied) != len(avail) {
		t.Errorf("applied=%d, want=%d (every embedded up-migration)", len(rep.Applied), len(avail))
	}
}
