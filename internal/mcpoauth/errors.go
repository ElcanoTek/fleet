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
// — the authorization server no longer recognizes our (dynamically registered)
// client. The caller may re-register via DCR and retry.
func IsInvalidClient(err error) bool {
	var oe *OAuthError
	return errors.As(err, &oe) && oe.Code == "invalid_client"
}
