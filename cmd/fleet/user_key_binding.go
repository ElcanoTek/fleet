// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/store"
)

// bindUserKeysAtBoot binds every user key minted before keys named their
// account (apikeys.Manager.BindUserKeys) against the identity table. Failures
// are logged, never fatal: an undecided key is bound on its first use. Nothing
// is bound once ctx has ended (the database is another server's now).
func bindUserKeysAtBoot(ctx context.Context, keys *apikeys.Manager, st *store.Store) {
	if ctx.Err() != nil {
		return
	}
	bound, missing, err := keys.BindUserKeys(ctx, accountLookup(st))
	switch {
	case err != nil:
		logging.From(ctx).Warn("apikeys: binding user keys to their accounts failed (each is bound on its first use instead)", "err", err)
	case bound+missing > 0:
		logging.From(ctx).Info("apikeys: bound user keys to their accounts", "bound", bound, "without_account", missing)
	}
}

// accountLookup finds an account's id in the identity table, whatever its
// state.
func accountLookup(st *store.Store) apikeys.AccountLookup {
	return func(ctx context.Context, email string) (string, bool, error) {
		u, err := st.Identity().GetByEmail(ctx, email)
		if errors.Is(err, identity.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return u.ID.String(), true, nil
	}
}
