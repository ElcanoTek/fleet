package httpapi

import (
	"context"
	"strings"

	"github.com/ElcanoTek/fleet/internal/principal"
)

// asUser is ctx carrying a resolved-looking session principal for email: what
// authMiddleware + the allow-all membership seam leave on a request, for tests
// that call a handler directly.
func asUser(ctx context.Context, email string) context.Context {
	return principal.With(ctx, principal.Principal{Email: email, AuthMethod: principal.MethodSession})
}

// asUserRole is asUser with the role membershipMiddleware resolves.
func asUserRole(ctx context.Context, email, role string) context.Context {
	return principal.With(ctx, principal.Principal{Email: email, Role: role, AuthMethod: principal.MethodSession})
}

// gatewayLegs are the routes only the gateway's own principal may call.
var gatewayLegs = map[string]bool{
	"/auth/verify":                  true,
	"/auth/session-epoch":           true,
	"/auth/external-session-epoch":  true,
	"/auth/external-session-revoke": true,
	"/auth/external-access":         true,
	"/auth/external-team-rename":    true,
}

// gatewayContext is the context the browser gateway calls path with on
// behalf of user: its own principal on a sign-in leg, a session principal
// otherwise ("" user = a session with no identity, which auth refuses).
func gatewayContext(path, user string) context.Context {
	ctx := context.Background()
	if p, _, _ := strings.Cut(path, "?"); gatewayLegs[p] {
		return principal.With(ctx, principal.Principal{Email: user, AuthMethod: principal.MethodGateway})
	}
	return principal.With(ctx, principal.Principal{Email: user, AuthMethod: principal.MethodSession})
}

// sessionContext is the gateway's context for a browser session carrying
// claim s.
func sessionContext(user string, s principal.Session) context.Context {
	return principal.With(context.Background(), principal.Principal{Email: user, AuthMethod: principal.MethodSession, Session: s})
}
