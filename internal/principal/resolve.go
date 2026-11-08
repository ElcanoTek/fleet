// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package principal

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/ElcanoTek/fleet/internal/identity"
)

// Directory is the account lookup Resolve needs. *identity.Store satisfies
// it: both backends read the same core.users row (#2049 M4), so there is one
// lookup and one session check, not one per plane.
type Directory interface {
	// GetByEmail returns the account row whatever its state, or
	// identity.ErrNotFound.
	GetByEmail(ctx context.Context, email string) (*identity.User, error)
	// LookupExternalSessionEpoch returns a central-Auth identity's live
	// session generation, "" when it never signed in here.
	LookupExternalSessionEpoch(ctx context.Context, issuer, subject string) (string, error)
}

var (
	// ErrNotMember means the account does not exist, or the plane's admit
	// rule refused it. Backends answer 403 {"error":"not_a_member"}.
	ErrNotMember = errors.New("principal: not a member")
	// ErrSessionRevoked means the session's epoch claim no longer matches the
	// account. Backends answer 401 {"error":"session_revoked"} with
	// X-Session-Revoked: 1, which makes the gateway drop the cookie.
	ErrSessionRevoked = errors.New("principal: session revoked")
	// ErrNoIdentity means the principal names no account to resolve.
	ErrNoIdentity = errors.New("principal: no identity")
)

// Admit is a plane's membership rule over the account row: chat admits an
// enabled account, the Operations Center one with Operations Center access
// (or, on the Prompt Library, any enabled account). nil admits any row.
type Admit func(*identity.User) bool

// Resolve is the one authentication lookup both backends run: it reads p's
// account row once, applies admit, checks a session principal's epoch claim,
// and returns p with UserID, Role and OpsScope taken from the row, together
// with the row itself (for the plane's own model of the user).
//
// Membership outranks the session: a deleted or refused account gets
// ErrNotMember, the verdict the UI can act on, never "sign in again". Any
// other error is a lookup failure, which a backend answers 500 — an
// unreachable database says nothing about the session, and the revoked
// verdict would delete a valid cookie over a blip.
//
// A session principal with no epoch claim is admitted: the elcano_auth
// cookie is minted by the auth service, which fleet cannot add a claim to, so
// it stays revocable there. The fleet-minted cookies this defends are refused
// by the gateway outright when the claim is missing.
func Resolve(ctx context.Context, dir Directory, p Principal, admit Admit) (Principal, *identity.User, error) {
	email := identity.NormalizeEmail(p.Email)
	if email == "" {
		return p, nil, ErrNoIdentity
	}
	u, err := dir.GetByEmail(ctx, email)
	if errors.Is(err, identity.ErrNotFound) {
		return p, nil, ErrNotMember
	}
	if err != nil {
		return p, nil, err
	}
	if admit != nil && !admit(u) {
		return p, u, ErrNotMember
	}
	if p.AuthMethod == MethodSession && p.Session.Epoch != "" {
		live := u.SessionEpoch
		if p.Session.Source == SourceOIDC {
			issuer, subject := strings.TrimSpace(p.Session.Issuer), strings.TrimSpace(p.Session.Subject)
			if issuer == "" || subject == "" {
				return p, u, ErrSessionRevoked
			}
			// Read-only: the row was created at the login mint. No row
			// means "", which matches no claim.
			if live, err = dir.LookupExternalSessionEpoch(ctx, issuer, subject); err != nil {
				return p, u, err
			}
		}
		if subtle.ConstantTimeCompare([]byte(p.Session.Epoch), []byte(live)) != 1 {
			return p, u, ErrSessionRevoked
		}
	}
	p.Email = email
	p.UserID = u.ID
	p.Role = string(u.Role)
	p.OpsScope = string(u.OpsScope)
	p.resolved = true
	return p, u, nil
}

// Enabled is the chat plane's Admit: the account exists and is enabled.
func Enabled(u *identity.User) bool { return u != nil && u.Enabled }
