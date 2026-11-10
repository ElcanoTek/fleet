// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeAccounts is an identity table keyed by stored (lowercase) email, with
// the same case-insensitive lookup identity.GetByEmail does.
type fakeAccounts struct {
	emails map[string]bool
	err    error
}

func (f fakeAccounts) GetIdentity(_ context.Context, email string) (*identity.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	e := identity.NormalizeEmail(email)
	if !f.emails[e] {
		return nil, identity.ErrNotFound
	}
	return &identity.User{Email: e}, nil
}

type fakeKeys map[string]*apikeys.APIKey

func (f fakeKeys) GetKey(keyID string) *apikeys.APIKey { return f[keyID] }

func TestResolvePrincipal(t *testing.T) {
	t.Parallel()
	accounts := fakeAccounts{emails: map[string]bool{"alice@example.com": true}}
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(24 * time.Hour)
	create := []models.Permission{models.PermissionCreateTask, models.PermissionViewTasks}
	keys := fakeKeys{
		"key_live":     {KeyID: "key_live", Enabled: true, Permissions: create},
		"key_expiring": {KeyID: "key_expiring", Enabled: true, Permissions: create, ExpiresAt: &future},
		"key_admin":    {KeyID: "key_admin", Enabled: true, Permissions: []models.Permission{models.PermissionAdmin}},
		"key_revoked":  {KeyID: "key_revoked", Enabled: false, Permissions: create},
		"key_expired":  {KeyID: "key_expired", Enabled: true, Permissions: create, ExpiresAt: &past},
		// Typed readonly / webhook / user keys carry no create_task.
		"key_readonly": {KeyID: "key_readonly", Enabled: true, Permissions: []models.Permission{models.PermissionViewTasks}},
		"key_user":     {KeyID: "key_user", Enabled: true, Type: apikeys.KeyTypeUser},
	}
	ctx := context.Background()

	cases := []struct {
		name      string
		scope     string
		principal string
		wantID    string // stored principal on success
		reason    string // want *InvalidPrincipalError with this Reason
	}{
		{name: "existing account", scope: models.BudgetScopeUser, principal: "alice@example.com", wantID: "alice@example.com"},
		// The QA case: a mixed-case email used to save verbatim and then never
		// match the lowercase creator username / usage bucket key.
		{name: "account email is canonicalised", scope: models.BudgetScopeUser, principal: "Alice@Example.COM", wantID: "alice@example.com"},
		// The QA case: a typo saved fine and listed as "ok" while bounding nothing.
		{name: "mistyped account", scope: models.BudgetScopeUser, principal: "alcie@example.com", reason: PrincipalNotFound},
		{name: "key that can create tasks", scope: models.BudgetScopeKey, principal: "key_live", wantID: "key_live"},
		{name: "key with a future expiry", scope: models.BudgetScopeKey, principal: "key_expiring", wantID: "key_expiring"},
		{name: "admin-permission key", scope: models.BudgetScopeKey, principal: "key_admin", wantID: "key_admin"},
		{name: "unknown key", scope: models.BudgetScopeKey, principal: "key_doesnotexist", reason: PrincipalNotFound},
		// Revocation is final, so such a budget could never bind again.
		{name: "revoked key", scope: models.BudgetScopeKey, principal: "key_revoked", reason: PrincipalInactive},
		{name: "expired key", scope: models.BudgetScopeKey, principal: "key_expired", reason: PrincipalInactive},
		{name: "readonly key", scope: models.BudgetScopeKey, principal: "key_readonly", reason: PrincipalCannotCreate},
		{name: "chat-only user key", scope: models.BudgetScopeKey, principal: "key_user", reason: PrincipalCannotCreate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bc := models.BudgetCreate{Scope: tc.scope, PrincipalID: tc.principal, Window: models.BudgetWindowDay}
			err := ResolvePrincipal(ctx, &bc, accounts, keys)
			if tc.reason != "" {
				var invalid *InvalidPrincipalError
				if !errors.As(err, &invalid) || invalid.Reason != tc.reason {
					t.Fatalf("got %v, want *InvalidPrincipalError with reason %q", err, tc.reason)
				}
				if !strings.Contains(err.Error(), tc.principal) {
					t.Errorf("message %q should name the principal %q", err.Error(), tc.principal)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if bc.PrincipalID != tc.wantID {
				t.Errorf("principal_id = %q, want %q", bc.PrincipalID, tc.wantID)
			}
		})
	}
}

// A check that could not run is not an invalid principal: the caller must not
// report the principal as bad, nor save the budget unverified, when the
// identity table could not be read or no key store is wired.
func TestResolvePrincipal_CannotCheck(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection reset")
	user := models.BudgetCreate{Scope: models.BudgetScopeUser, PrincipalID: "alice@example.com", Window: models.BudgetWindowDay}
	key := models.BudgetCreate{Scope: models.BudgetScopeKey, PrincipalID: "key_live", Window: models.BudgetWindowDay}
	for name, err := range map[string]error{
		"account lookup failed": ResolvePrincipal(context.Background(), &user, fakeAccounts{err: boom}, fakeKeys{}),
		"no key store":          ResolvePrincipal(context.Background(), &key, fakeAccounts{}, nil),
	} {
		var invalid *InvalidPrincipalError
		if err == nil || errors.As(err, &invalid) {
			t.Errorf("%s: got %v, want a non-InvalidPrincipalError error", name, err)
		}
	}
}

func TestInvalidPrincipalErrorMessages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  InvalidPrincipalError
		want []string
	}{
		{InvalidPrincipalError{Scope: models.BudgetScopeUser, PrincipalID: "alcie@example.com", Reason: PrincipalNotFound},
			[]string{`no account with email "alcie@example.com"`}},
		{InvalidPrincipalError{Scope: models.BudgetScopeKey, PrincipalID: "key_x", Reason: PrincipalNotFound},
			[]string{`no API key with key_id "key_x"`, "fleet sched apikey list"}},
		{InvalidPrincipalError{Scope: models.BudgetScopeKey, PrincipalID: "key_x", Reason: PrincipalInactive},
			[]string{`API key "key_x" is revoked or expired`}},
		{InvalidPrincipalError{Scope: models.BudgetScopeKey, PrincipalID: "key_x", Reason: PrincipalCannotCreate},
			[]string{`API key "key_x" cannot create tasks`, "create_task"}},
	} {
		msg := tc.err.Error()
		for _, want := range tc.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%+v: message %q missing %q", tc.err, msg, want)
			}
		}
	}
}
