// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/store"
)

// bindUserKeysAtBoot binds every user key minted before keys named their
// account (apikeys.Manager.BindUserKeys) against the identity table. Failures
// are logged, never fatal: an undecided key is bound on its first use.
func bindUserKeysAtBoot(keys *apikeys.Manager, st *store.Store) {
	bound, missing, err := keys.BindUserKeys(context.Background(), accountLookup(st))
	switch {
	case err != nil:
		slog.Warn("apikeys: binding user keys to their accounts failed (each is bound on its first use instead)", "err", err)
	case bound+missing > 0:
		slog.Info("apikeys: bound user keys to their accounts", "bound", bound, "without_account", missing)
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
