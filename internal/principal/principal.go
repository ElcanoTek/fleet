// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package principal is the typed identity of an HTTP request (#2050, ADR-0108).
//
// Before it existed, identity travelled between fleet's own in-process layers
// as request headers: the browser gateway (internal/webui) verified the
// session cookie and then wrote X-User-Email, the session-epoch claim and the
// central-Auth issuer/subject onto a fresh header set for each backend call,
// next to a shared server token that told the backend to believe them. Each
// backend re-read those headers in its own auth stack.
//
// A Principal replaces that channel. It lives in the request context, which
// no network peer can write: net/http starts every inbound request with a
// fresh context, so the only way a Principal reaches a handler is that fleet
// code put it there — the gateway for a browser session, or a listener's own
// edge middleware for a credential it verified itself (an API key). Backends
// read identity from here and nowhere else; TestNoIdentityHeaderReaders fails
// the build on a non-test read of an identity header.
//
// A Principal arrives in two states. An ASSERTED principal is what the
// gateway knows from the cookie alone: the email and the session's epoch
// claim. The backend still decides whether that account exists, is enabled,
// may use this plane, and whether the session has been revoked; Resolve does
// that in one lookup of the account's single row in core.users and returns
// the principal with UserID, Role and OpsScope filled in.
package principal

import (
	"context"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// AuthMethod is how a request proved who it is. Rate limiting and CSRF key
// off it, so a new method must be placed in both (see IsBrowserSession and
// IsCredential).
type AuthMethod string

const (
	// MethodSession is a browser session the gateway verified (the HMAC
	// elcano_session cookie or the Ed25519 elcano_auth cookie). The gateway
	// enforced the request's Origin before it attached the principal.
	MethodSession AuthMethod = "session"
	// MethodGateway is the gateway itself calling a backend in-process with
	// no browser session behind it: the pre-session sign-in legs (Email is
	// the address being signed in), a verified identity-provider event, and
	// the identity-less brand assets (Email is empty).
	MethodGateway AuthMethod = "gateway"
	// MethodAPIKey is a fleet API key presented as X-API-Key.
	MethodAPIKey AuthMethod = "api_key"
	// MethodBearer is a fleet API key presented as Authorization: Bearer.
	MethodBearer AuthMethod = "bearer"
	// MethodAdminKey is the bootstrap ADMIN_API_KEY.
	MethodAdminKey AuthMethod = "admin_key"
	// MethodServiceToken is the legacy loopback channel: a caller holding the
	// shared FLEET_SERVER_TOKEN asserting a user by header on a backend's own
	// listener (`fleet chat`, `fleet acp`, fleet-bench). Only the listeners'
	// edge middleware produces it.
	MethodServiceToken AuthMethod = "service_token"
)

// Session is the session claim a MethodSession principal carries: what the
// gateway read out of the cookie it verified. Resolve compares Epoch with
// the account's live epoch, which is what makes a password change (or a
// central-Auth revocation) end the sessions minted before it.
type Session struct {
	// Epoch is the cookie's session-epoch claim; "" for a cookie that
	// carries none (the elcano_auth cookie is revocable at the auth service).
	Epoch string
	// Source is the session's origin: "oidc" for a central-Auth (OIDC)
	// session, whose epoch is keyed by Issuer and Subject rather than the
	// account row; anything else for a password or elcano_auth session.
	Source string
	// Issuer and Subject are the central-Auth identity (Source "oidc" only).
	Issuer  string
	Subject string
}

// SourceOIDC is Session.Source for a central-Auth session.
const SourceOIDC = "oidc"

// Principal is who a request is, and how it proved it.
type Principal struct {
	// Email is the account's normalised address; "" for an identity-less
	// gateway call or an API key that names no user.
	Email string
	// UserID is the account's core.users id; uuid.Nil until Resolve.
	UserID uuid.UUID
	// Role is the account's Fleet role (admin | member | viewer); "" until
	// Resolve. "" is never treated as admin.
	Role string
	// OpsScope is the account's Operations Center scope (none | view | run);
	// "" until Resolve.
	OpsScope string
	// Scopes are an API key's permissions; nil for a person.
	Scopes []string
	// KeyID is the API key's id, for an API-key or bearer principal.
	KeyID string
	// AuthMethod is how the request authenticated.
	AuthMethod AuthMethod
	// Session is the session claim of a MethodSession principal.
	Session Session
	// resolved is set by Resolve: the account lookup ran and admitted it.
	resolved bool
}

// Resolved reports whether Resolve admitted this principal (its UserID and
// Role come from the account row, not from the caller).
func (p Principal) Resolved() bool { return p.resolved }

// IsBrowserSession reports whether the principal is the gateway acting for a
// browser (a session or a gateway-internal call). Such a request already
// passed the gateway's CSRF Origin check, and a backend must not repeat it:
// the gateway does not forward the browser's Origin.
func (p Principal) IsBrowserSession() bool {
	return p.AuthMethod == MethodSession || p.AuthMethod == MethodGateway
}

// IsCredential reports whether the principal presented a credential of its
// own (an API key, a bearer, the admin key): a programmatic caller, not a
// browser, so a cross-site request cannot carry it.
func (p Principal) IsCredential() bool {
	switch p.AuthMethod {
	case MethodAPIKey, MethodBearer, MethodAdminKey, MethodServiceToken:
		return true
	case MethodSession, MethodGateway:
		return false
	}
	return false
}

type ctxKey struct{}

// With returns ctx carrying p, replacing any principal ctx already held. It
// also names the principal on the context's log correlation (user_id,
// LogIdentity), so every line logged while serving the request says who it
// is for — wherever the principal is attached: the gateway, a backend's edge,
// the admin-key and API-key paths (#2052).
func With(ctx context.Context, p Principal) context.Context {
	ctx = context.WithValue(ctx, ctxKey{}, p)
	if id := p.LogIdentity(); id != "" {
		ctx = logging.WithUser(ctx, id)
	}
	return ctx
}

// LogIdentity is the principal's user_id on log lines: the bootstrap admin
// key is "admin", an API key or bearer "apikey/<key id>" — its id, never the
// key; the separator is "/", not ":", because "apikey:<value>" is the
// marker=value shape the log redactor scrubs — and a person their account
// email. "" for an identity-less gateway call.
func (p Principal) LogIdentity() string {
	switch {
	case p.AuthMethod == MethodAdminKey:
		return "admin"
	case (p.AuthMethod == MethodAPIKey || p.AuthMethod == MethodBearer) && p.KeyID != "":
		return "apikey/" + p.KeyID
	default:
		return p.Email
	}
}

// From returns the principal ctx carries, and whether it carries one.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Without returns ctx with no principal: a backend call made on a request's
// context for some other purpose (an identity-less asset) must not inherit
// the caller's identity by accident.
func Without(ctx context.Context) context.Context {
	if _, ok := From(ctx); !ok {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, nil)
}

// Email is the principal's email, or "" when ctx carries none.
func Email(ctx context.Context) string {
	p, _ := From(ctx)
	return p.Email
}
