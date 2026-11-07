package httpapi

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// callerIsAdmin is the one rule every context-based admin gate applies: the
// ADMIN_EMAILS allowlist OR a users.role = 'admin' account, with the
// allowlist ignored where central Auth manages accounts.
func TestCallerIsAdmin(t *testing.T) {
	t.Parallel()

	ctxFor := func(email, role string) context.Context {
		ctx := context.WithValue(context.Background(), ctxKeyUser, email)
		if role != "" {
			ctx = context.WithValue(ctx, ctxKeyRole, role)
		}
		return ctx
	}
	cases := []struct {
		name        string
		authManaged bool
		email, role string
		want        bool
	}{
		{"env allowlist", false, "boot@x.com", store.RoleMember, true},
		{"env allowlist, case-insensitive", false, "Boot@X.com", "", true},
		{"db admin role", false, "other@x.com", store.RoleAdmin, true},
		{"plain member", false, "other@x.com", store.RoleMember, false},
		{"no role at all", false, "other@x.com", "", false},
		{"auth-managed ignores the allowlist", true, "boot@x.com", store.RoleMember, false},
		{"auth-managed keeps the db role", true, "other@x.com", store.RoleAdmin, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: &config.Config{AdminEmails: []string{"boot@x.com"}, AccountsManagedByAuth: tc.authManaged}}
			if got := s.callerIsAdmin(ctxFor(tc.email, tc.role)); got != tc.want {
				t.Errorf("callerIsAdmin = %v, want %v", got, tc.want)
			}
		})
	}
}
