// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// The Operations Center's model of an account.
//
// The browser tier (internal/webui) verifies every session, the elcano_auth
// cookie included, and calls this server in-process with the session's
// principal (user_principal.go); this server never reads that cookie itself.
// Sign-out is the gateway's /api/auth/logout: the Operations Center has no
// session of its own to end since its password bearer was retired (#2049 M4,
// ADR-0105).

package handlers

import (
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// opsModel renders an identity row with Operations Center access as this
// plane's user (Role = the legacy Operations Center role).
func opsModel(u *identity.User) *models.User {
	return &models.User{
		ID:       u.ID,
		Username: u.Email,
		Role:     u.LegacyOpsRole(),
	}
}
