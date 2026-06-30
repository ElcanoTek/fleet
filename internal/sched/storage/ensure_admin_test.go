package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestEnsureAdminUser locks in the #458 bootstrap-admin primitive: it creates a
// missing user as admin, promotes an existing non-admin, leaves an existing
// admin untouched, and treats a blank username as a no-op. This is what lets a
// configured operator reach the Operations Center through the chat session
// cookie without a manual `fleet-admin sched user add`.
func TestEnsureAdminUser(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// 1. Missing user → created as admin with a non-empty (random) password hash.
	const created = "bootstrap@example.com"
	if err := store.EnsureAdminUser(ctx, created); err != nil {
		t.Fatalf("EnsureAdminUser(create): %v", err)
	}
	u, err := store.GetUserByUsername(created)
	if err != nil || u == nil {
		t.Fatalf("expected created user, got %v (err %v)", u, err)
	}
	if u.Role != "admin" {
		t.Errorf("created role = %q, want admin", u.Role)
	}
	if u.PasswordHash == "" {
		t.Error("created user must have a (random, unusable) password hash, got empty")
	}

	// Idempotent: a second ensure of the same admin keeps the same row + role.
	if err := store.EnsureAdminUser(ctx, created); err != nil {
		t.Fatalf("EnsureAdminUser(idempotent): %v", err)
	}
	if again, _ := store.GetUserByUsername(created); again == nil || again.ID != u.ID || again.Role != "admin" {
		t.Errorf("idempotent ensure changed the row: %+v (was id=%s)", again, u.ID)
	}

	// 2. Existing non-admin → promoted to admin, same row.
	const demoted = "client@example.com"
	client := &models.User{ID: uuid.New(), Username: demoted, Role: "client", PasswordHash: "x", CreatedAt: u.CreatedAt}
	if _, err := store.AddUser(client); err != nil {
		t.Fatalf("AddUser(client): %v", err)
	}
	if err := store.EnsureAdminUser(ctx, demoted); err != nil {
		t.Fatalf("EnsureAdminUser(promote): %v", err)
	}
	promoted, _ := store.GetUserByUsername(demoted)
	if promoted == nil || promoted.ID != client.ID {
		t.Fatalf("promote replaced the row: %+v (was id=%s)", promoted, client.ID)
	}
	if promoted.Role != "admin" {
		t.Errorf("promoted role = %q, want admin", promoted.Role)
	}

	// 3. Email is case-folded to match lookupMember's lowercased lookup.
	if err := store.EnsureAdminUser(ctx, "  MixedCase@Example.com "); err != nil {
		t.Fatalf("EnsureAdminUser(mixedcase): %v", err)
	}
	if mc, _ := store.GetUserByUsername("mixedcase@example.com"); mc == nil || mc.Role != "admin" {
		t.Errorf("expected lowercased admin row for mixed-case input, got %+v", mc)
	}

	// 4. Blank username is a no-op (no error, no row).
	if err := store.EnsureAdminUser(ctx, "   "); err != nil {
		t.Errorf("EnsureAdminUser(blank) should be a no-op, got %v", err)
	}
}
