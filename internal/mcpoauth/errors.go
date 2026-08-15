package mcpoauth

import (
	"errors"
	"fmt"
)

// OAuthError is a parsed RFC 6749 §5.2 token-endpoint error response. The
// machine-readable Code drives control flow: invalid_grant means the refresh
// token is dead (re-auth required), invalid_target means the authorization
// server doesn't honor the RFC 8707 resource indicator (retry without it).
type OAuthError struct {
	Code        string // e.g. "invalid_grant", "invalid_target", "invalid_client"
	Description string // optional human-readable detail (safe to surface; never contains the token)
	HTTPStatus  int
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("oauth error %q: %s", e.Code, e.Description)
	}
	return fmt.Sprintf("oauth error %q (http %d)", e.Code, e.HTTPStatus)
}

// IsInvalidGrant reports whether err is an OAuthError with code invalid_grant —
// the signal that a refresh token has been revoked/expired/rotated away and the
// connection needs the user to re-authorize. Callers mark the connection
// needs-reauth and degrade gracefully rather than failing the whole run.
func IsInvalidGrant(err error) bool {
	var oe *OAuthError
	return errors.As(err, &oe) && oe.Code == "invalid_grant"
}

// IsInvalidTarget reports whether err is an OAuthError with code invalid_target
// — the authorization server rejected the RFC 8707 resource parameter. The MCP
// spec says send resource regardless, but real IdPs (e.g. Entra v2) reject it,
// so on this error the caller retries the request WITHOUT resource and relies on
// scope-based audience instead.
func IsInvalidTarget(err error) bool {
	var oe *OAuthError
	return errors.As(err, &oe) && oe.Code == "invalid_target"
}

// IsInvalidClient reports whether err is an OAuthError with code invalid_client
// — the authorization server no longer recognizes our client credentials. For a
// DCR-registered client this usually means the registration was pruned or
// expired server-side; for a BYO client it means the id/secret is wrong or was
// rotated. Either way the stored registration is unusable, so this is terminal
// for refresh (see IsTerminalRefreshError): the connection is marked needs-reauth
// and reconnecting re-runs registration through the normal connect flow.
func IsInvalidClient(err error) bool {
	var oe *OAuthError
	return errors.As(err, &oe) && oe.Code == "invalid_client"
}

// IsInvalidScope reports whether err is an OAuthError with code invalid_scope —
// the authorization server rejected the requested scope. On refresh this is
// recoverable: RFC 6749 §6 makes `scope` OPTIONAL and defines its omission as
// "identical to the scope originally granted", so the caller retries without it
// (see FlowConfig.Refresh). It matters because fleet stores the scopes it
// REQUESTED at connect time, which an AS is free to narrow when it grants — and
// asking to refresh a wider scope than was granted is an error per §6.
func IsInvalidScope(err error) bool {
	var oe *OAuthError
	return errors.As(err, &oe) && oe.Code == "invalid_scope"
}

// IsTerminalRefreshError reports whether err means "re-issuing this same refresh
// request will never succeed", so the caller must stop retrying and hand the
// connection back to the user as needs-reauth rather than failing every run with
// the same opaque error forever.
//
// The terminal set is deliberately narrow — the three RFC 6749 §5.2 codes that
// condemn the stored grant or client registration itself:
//
//   - invalid_grant        the refresh token is revoked/expired/already rotated
//   - invalid_client       the client credentials are no longer recognized
//   - unauthorized_client  this client may not use the refresh_token grant
//
// Everything else stays transient (the caller rolls back and retries later):
// network failures, 5xx, and invalid_scope — which Refresh recovers from on its
// own by retrying without the scope parameter.
func IsTerminalRefreshError(err error) bool {
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return false
	}
	switch oe.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client":
		return true
	default:
		return false
	}
}

// ReauthDetail renders a short, user-facing explanation of why a connection was
// marked needs-reauth. It is shown in the connections UI, so it names the cause
// rather than assuming expiry. Never includes the AS's error_description, which
// is attacker-influenced free text from a user-supplied server.
func ReauthDetail(err error) string {
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return "authorization expired — reconnect required"
	}
	switch oe.Code {
	case "invalid_client":
		return "the authorization server no longer recognizes this client — reconnect required"
	case "unauthorized_client":
		return "this client is not permitted to refresh access — reconnect required"
	default:
		return "authorization expired — reconnect required"
	}
}
