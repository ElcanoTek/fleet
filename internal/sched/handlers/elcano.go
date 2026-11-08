// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// The membership lookup the header-trust path gates on.
//
// The browser tier (internal/webui) verifies every session, the elcano_auth
// cookie included, and forwards a trusted identity here (header trust,
// middleware.go); this server never reads that cookie itself. Sign-out is the
// gateway's /api/auth/logout: the Operations Center has no session of its own
// to end since its password bearer was retired (#2049 M4, ADR-0105).

package handlers

import (
	"context"
	"errors"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// lookupMember resolves an email to its identity row, whatever its state (nil
// when there is none); headerTrustMember decides what it grants. It is the ONE
// row in core.users (#2049 M4): there is no second plane to ask.
func (h *Handlers) lookupMember(ctx context.Context, email string) (*identity.User, error) {
	id, err := h.storage.GetIdentity(ctx, email)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, nil
	}
	return id, err
}

// opsModel renders an identity row with Operations Center access as this
// plane's user (Role = the legacy Operations Center role).
func opsModel(u *identity.User) *models.User {
	return &models.User{
		ID:       u.ID,
		Username: u.Email,
		Role:     u.LegacyOpsRole(),
	}
}
