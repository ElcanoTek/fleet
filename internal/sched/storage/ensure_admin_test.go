package storage

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/identity"
)

// TestEnsureAdmin locks in the #458 bootstrap-admin primitive over the one
// identity table (#2049 M4): it creates a missing account as admin (with no
// usable password: a bootstrap admin signs in with a session cookie),
// promotes an existing non-admin, re-enables a disabled one, leaves an
// existing admin untouched, and treats a blank email as a no-op.
func TestEnsureAdmin(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()
	ids := store.Identity()

	const created = "Bootstrap@Example.com"
	if err := ids.EnsureAdmin(ctx, created); err != nil {
		t.Fatalf("EnsureAdmin(create): %v", err)
	}
	u, err := store.GetUserByUsername("bootstrap@example.com")
	if err != nil || u.Role != "admin" {
		t.Fatalf("created = %+v, %v; want an admin", u, err)
	}
	var hash string
	if err := store.DB().Conn().QueryRowContext(ctx, `SELECT password_hash FROM core.users WHERE email = 'bootstrap@example.com'`).Scan(&hash); err != nil || hash != identity.UnusablePasswordHash {
		t.Errorf("created password hash = %q, %v; want the unusable sentinel", hash, err)
	}
	firstID := u.ID
	if err := ids.EnsureAdmin(ctx, created); err != nil {
		t.Fatalf("EnsureAdmin(again): %v", err)
	}
	if again, err := store.GetUserByUsername("bootstrap@example.com"); err != nil || again.ID != firstID {
		t.Fatalf("second ensure changed the row: %+v %v", again, err)
	}

	// An existing member (ops_scope view) is promoted; a disabled one is
	// re-enabled — the boot seed is config-authoritative.
	if _, err := ids.CreateAccount(ctx, identity.NewAccount{Email: "member@example.com", Role: identity.RoleMember, OpsScope: identity.OpsView}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Conn().ExecContext(ctx, `UPDATE core.users SET enabled = FALSE WHERE email = 'member@example.com'`); err != nil {
		t.Fatal(err)
	}
	if err := ids.EnsureAdmin(ctx, "member@example.com"); err != nil {
		t.Fatal(err)
	}
	if acct, err := ids.GetByEmail(ctx, "member@example.com"); err != nil || acct.Role != identity.RoleAdmin || !acct.Enabled {
		t.Fatalf("promoted = %+v, %v", acct, err)
	}

	if err := ids.EnsureAdmin(ctx, "   "); err != nil {
		t.Fatalf("blank email must be a no-op: %v", err)
	}
}

// TestOpsRoleReportsEnabledRoleOnly pins the export's Ops role read: a
// missing row, a chat-only account and a centrally disabled account all read
// as "none".
func TestOpsRoleReportsEnabledRoleOnly(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()
	const email = "Ops-Role@Example.com"
	if role, err := store.OpsRole(ctx, email); err != nil || role != "none" {
		t.Fatalf("missing row = (%q, %v), want none", role, err)
	}
	if _, err := store.Identity().CreateAccount(ctx, identity.NewAccount{Email: email, Role: identity.RoleMember}); err != nil {
		t.Fatal(err)
	}
	if role, err := store.OpsRole(ctx, email); err != nil || role != "none" {
		t.Fatalf("chat-only row = (%q, %v), want none", role, err)
	}
	if _, err := store.Identity().SetOpsAccess(ctx, email, "client"); err != nil {
		t.Fatal(err)
	}
	if role, err := store.OpsRole(ctx, email); err != nil || role != "client" {
		t.Fatalf("enabled row = (%q, %v), want client", role, err)
	}
	if _, err := store.DB().Conn().ExecContext(ctx, `UPDATE core.users SET enabled = FALSE WHERE email = 'ops-role@example.com'`); err != nil {
		t.Fatal(err)
	}
	if role, err := store.OpsRole(ctx, email); err != nil || role != "none" {
		t.Fatalf("disabled row = (%q, %v), want none", role, err)
	}
}
