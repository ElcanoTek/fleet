package store

import (
	"context"
	"testing"
)

func TestAccountAccessIncludesDisabledRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, found, err := st.AccountAccess(ctx, "nobody@x.com"); err != nil || found {
		t.Fatalf("missing account = (%v, %v), want not found", found, err)
	}
	if _, err := st.CreateUser(ctx, "d@x.com", "password-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE users SET enabled = FALSE WHERE email = 'd@x.com'`); err != nil {
		t.Fatal(err)
	}
	a, found, err := st.AccountAccess(ctx, "D@x.com")
	if err != nil || !found || a.Enabled || a.Role != RoleMember {
		t.Fatalf("disabled account = (%+v, %v, %v), want found, disabled, member", a, found, err)
	}
	all, err := st.ListAccountAccess(ctx)
	if err != nil || len(all) != 1 || all[0].Email != "d@x.com" {
		t.Fatalf("list = (%+v, %v)", all, err)
	}
}
