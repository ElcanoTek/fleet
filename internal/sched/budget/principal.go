// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package budget

// Principal resolution for budget writes (#2105 QA). A budget binds to its
// principal by string equality — CheckCreate looks budgets up by the
// creator's username or key id, and spend is summed under the part-1 usage
// bucket keys — so a budget whose principal_id names no account or key can
// never match anything. Before this check such a row saved fine and listed as
// "ok" with $0 spend: an admin who mistyped an email believed a spend limit
// was in place when nothing was bounded. Both write paths (POST
// /admin/budgets and `fleet sched budget create`) resolve the principal here,
// so they cannot drift on what they accept.

import (
	"context"
	"errors"
	"fmt"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// AccountLookup finds an account by email whatever its access or state,
// satisfied by *storage.Storage. identity.ErrNotFound means no such account.
type AccountLookup interface {
	GetIdentity(ctx context.Context, email string) (*identity.User, error)
}

// KeyLookup finds an API key by key_id, satisfied by *apikeys.Manager. A nil
// result means no such key.
type KeyLookup interface {
	GetKey(keyID string) *apikeys.APIKey
}

// Why a budget write's principal was refused.
const (
	// PrincipalNotFound: no account with that email / no key with that key_id.
	PrincipalNotFound = "not_found"
	// PrincipalInactive: the key is revoked or expired. Revocation is final
	// (nothing re-enables a key), so the budget could never bind again.
	PrincipalInactive = "inactive"
	// PrincipalCannotCreate: the key has no create_task permission (a
	// readonly, webhook or chat-only user key), so it never reaches the
	// task-create paths budgets gate.
	PrincipalCannotCreate = "cannot_create"
)

// InvalidPrincipalError refuses a budget write whose principal_id could never
// match a task create: an unknown account or key, or a key that cannot create
// tasks. Write paths map it to a 400 / a CLI usage error, with Error() as the
// message.
type InvalidPrincipalError struct {
	Scope       string
	PrincipalID string
	Reason      string // one of the Principal* constants
}

func (e *InvalidPrincipalError) Error() string {
	switch {
	case e.Scope == models.BudgetScopeKey && e.Reason == PrincipalInactive:
		return fmt.Sprintf("API key %q is revoked or expired, so it can create no tasks and a budget on it "+
			"would bound nothing", e.PrincipalID)
	case e.Scope == models.BudgetScopeKey && e.Reason == PrincipalCannotCreate:
		return fmt.Sprintf("API key %q cannot create tasks (it has no create_task permission), so a budget on it "+
			"would bound nothing: budgets gate task creation", e.PrincipalID)
	case e.Scope == models.BudgetScopeKey:
		return fmt.Sprintf("no API key with key_id %q: a key budget's principal_id is the key's key_id, "+
			"as GET /keys and `fleet sched apikey list` show it", e.PrincipalID)
	default:
		return fmt.Sprintf("no account with email %q: a user budget's principal_id is an existing account's email "+
			"(Settings → Admin → Users)", e.PrincipalID)
	}
}

// ResolvePrincipal checks that bc.PrincipalID names a principal a budget can
// bind to for bc.Scope, and rewrites a user principal to the account's stored
// email. Stored emails are lowercase, and so are the creator usernames and
// usage bucket keys a budget is matched against — so "Alice@Example.com"
// would save and then never match; resolving it to the account's email makes
// it bind.
//
// Call it after bc.Validate(). It returns *InvalidPrincipalError when the
// principal does not exist or (scope key) can never create a task, and any
// other error when the check itself could not run — a failed account lookup,
// or a key budget with no key store to check it against (keys == nil). The
// caller must not save the budget unverified.
//
// A disabled account still resolves: a budget on it is valid policy that
// applies again if the account is re-enabled. A key is different — it binds
// only while it can create tasks, and revocation is final.
func ResolvePrincipal(ctx context.Context, bc *models.BudgetCreate, accounts AccountLookup, keys KeyLookup) error {
	switch bc.Scope {
	case models.BudgetScopeUser:
		u, err := accounts.GetIdentity(ctx, bc.PrincipalID)
		if errors.Is(err, identity.ErrNotFound) {
			return &InvalidPrincipalError{Scope: bc.Scope, PrincipalID: bc.PrincipalID, Reason: PrincipalNotFound}
		}
		if err != nil {
			return fmt.Errorf("look up account %q: %w", bc.PrincipalID, err)
		}
		bc.PrincipalID = u.Email
	case models.BudgetScopeKey:
		if keys == nil {
			return fmt.Errorf("no API key store to check key %q against", bc.PrincipalID)
		}
		k := keys.GetKey(bc.PrincipalID)
		switch {
		case k == nil:
			return &InvalidPrincipalError{Scope: bc.Scope, PrincipalID: bc.PrincipalID, Reason: PrincipalNotFound}
		case !k.IsValid():
			return &InvalidPrincipalError{Scope: bc.Scope, PrincipalID: bc.PrincipalID, Reason: PrincipalInactive}
		case !k.HasPermission(models.PermissionCreateTask):
			return &InvalidPrincipalError{Scope: bc.Scope, PrincipalID: bc.PrincipalID, Reason: PrincipalCannotCreate}
		}
	}
	return nil
}
