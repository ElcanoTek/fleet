// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The Operations Center's own password sign-in (POST /auth/login) and the
// bearer token it minted are retired (#2049 M4, ADR-0105): people reach the
// Operations Center with the one Fleet session cookie (header trust,
// middleware.go), and programs with API keys.

// CreateUser handles POST /users (admin key only): give an account
// Operations Center access. The account is a row of the one identity table
// (internal/identity): a new email becomes a new account with role viewer —
// so the person can also sign in to chat, read-only — and the requested
// Operations Center access. The request still speaks the legacy roles:
// admin | client | readonly. "admin" makes the account a Fleet admin, since
// Operations Center admin is the Fleet admin role.
//
// An email that is already an account is refused (409), as an existing
// username was: changing an existing account's access is the admin Users
// page's or `fleet sched user set-role`'s job.
func (h *Handlers) CreateUser(w http.ResponseWriter, r *http.Request) {
	if h.config.AccountsManagedByAuth {
		writeError(w, http.StatusConflict, "This Fleet's accounts are managed in Auth. Grant Fleet and its Ops Center role there.")
		return
	}
	var req models.UserCreate
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	username := identity.NormalizeEmail(req.Username)
	if len(username) < 3 || len(username) > 64 {
		writeError(w, http.StatusBadRequest, "Username must be between 3 and 64 characters")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "Password must be at least 8 characters")
		return
	}
	scope, admin, ok := identity.ParseOpsRole(req.Role)
	if !ok || req.Role == identity.LegacyOpsNone {
		writeError(w, http.StatusBadRequest, "Invalid role. Must be one of: admin, client, readonly")
		return
	}
	hash, err := identity.HashPassword(req.Password, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to hash password")
		return
	}
	acct := identity.NewAccount{Email: username, PasswordHash: hash, Role: identity.RoleViewer, OpsScope: scope}
	if admin {
		acct.Role, acct.OpsScope = identity.RoleAdmin, identity.OpsNone
	}
	u, err := h.storage.Identity().CreateAccount(r.Context(), acct)
	if err != nil {
		if errors.Is(err, identity.ErrExists) {
			writeError(w, http.StatusConflict, "Username already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to create user")
		return
	}
	writeJSON(w, http.StatusCreated, models.UserResponse{
		ID:        u.ID,
		Username:  u.Email,
		Role:      u.LegacyOpsRole(),
		CreatedAt: time.Unix(u.CreatedAt, 0).UTC(),
	})
}
