package agentcore

import (
	"strings"

	"github.com/ElcanoTek/fleet/internal/usertoken"
)

// Reserved ${FLEET_USER_EMAIL} manifest-env token (ADR-0087,
// docs/MCP-BUNDLE-ENV.md §1b).
//
// A bundle connector that writes something addressed to a person — a deal-sheet
// email whose recipient defaults to the trader who asked for it — needs to
// know who that person is. Nothing else in a server's env can tell it: the
// operator's process env is deployment-wide, and the model's arguments are
// model-authored text, which is exactly the wrong place to learn whose turn
// this is. This token lets a bundle ask for it in an mcp_servers[].env value
// (stdio servers only, like the workspace tokens):
//
//	env:
//	  DEAL_SHEET_DEFAULT_TO: "${FLEET_USER_EMAIL}"
//
// The value comes from ONE place: the authenticated user who sent an
// interactive chat turn (or, for an approved card, the user who approved it —
// the same person, since an approval is claimable only by its own user). It
// is resolved in the parent from the request's authenticated identity, never
// from the conversation, the model, or the bundle, crosses to the credential
// owner as ScopeSpec.User, which that child stamps on the turn scope's bases
// (MCPServerBase.User), and the binder substitutes it last, at spawn — after
// the account overlay and its identity guard. Every other spawn path — the
// boot catalog, hot reload, load-on-demand onto a shared client,
// `fleet mcp test`, and every scheduled run, dedicated client or not — has no
// signed-in user and DROPS each token-bearing key, so the server sees the
// variable as unset. A key is never handed a literal token, an empty string,
// or another user's identity.
//
// There is no name token. Fleet's sign-in carries an email and no name, and
// a name is never guessed from the email, so FLEET_USER_NAME is reserved and
// refused (usertoken.ReservedNameVar): the loader fails a bundle that writes
// it and the binder drops any key that still carries it. A connector that
// greets someone uses its own no-name wording ("Hi team,").
//
// The value is data, not instructions, and not authorization: a server must
// never treat it as a credential or a seat selector. It is validated against a
// conservative character set on BOTH sides of the broker boundary (the child
// does not trust the parent's formatting), and a value that fails is treated
// as absent — dropped, not passed through. The set admits no '$', '{' or '}',
// so a substituted value can never form another reserved token for a later
// expansion pass to resolve.
const (
	// UserEmailEnvToken resolves to the turn sender's normalized (lowercased)
	// email address.
	UserEmailEnvToken = usertoken.EmailToken

	// maxMCPUserEmailLen is RFC 5321's path limit.
	maxMCPUserEmailLen = 254
)

// MCPUserIdentity is the signed-in user an interactive MCP scope is opened
// for. The zero value means "no user" — the only value any non-interactive
// spawn path may use.
type MCPUserIdentity struct {
	Email string
}

// IsZero reports whether the identity carries nothing usable.
func (u MCPUserIdentity) IsZero() bool { return u.Email == "" }

// NewMCPUserIdentity builds the identity for an authenticated user: the
// normalized email when it passes ValidMCPUserEmail, else the zero identity.
func NewMCPUserIdentity(email string) MCPUserIdentity {
	return MCPUserIdentity{Email: email}.Sanitized()
}

// Sanitized returns u with every field that fails validation cleared. The
// credential owner calls it on the identity it receives over the wire.
func (u MCPUserIdentity) Sanitized() MCPUserIdentity {
	if email := strings.ToLower(strings.TrimSpace(u.Email)); ValidMCPUserEmail(email) {
		return MCPUserIdentity{Email: email}
	}
	return MCPUserIdentity{}
}

// ValidMCPUserEmail reports whether s is a plain address fleet will hand to a
// connector: one '@', a non-empty local part of ASCII letters, digits and
// . _ % + - ', and a dotted domain of ASCII letters, digits and '-'. That is
// deliberately narrower than RFC 5322 — a quoted local part, a comment, or a
// '$' / '{' / '}' (which could spell another reserved token) is refused.
func ValidMCPUserEmail(s string) bool {
	if len(s) < 3 || len(s) > maxMCPUserEmailLen {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') || at == len(s)-1 {
		return false
	}
	for i := 0; i < at; i++ {
		c := s[i]
		if !isASCIIAlnum(c) && !strings.ContainsRune("._%+-'", rune(c)) {
			return false
		}
	}
	domain := s[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return false
	}
	for i := 0; i < len(domain); i++ {
		c := domain[i]
		if !isASCIIAlnum(c) && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// EnvReferencesUser reports whether any value in env names a reserved user
// variable, in any spelling (usertoken.Find: exact, escaped, padded with any
// whitespace or invisible character, a colon form, or the refused
// FLEET_USER_NAME).
func EnvReferencesUser(env map[string]string) bool {
	for _, v := range env {
		if usertoken.Mentions(v) {
			return true
		}
	}
	return false
}

// ExpandUserEnv resolves ${FLEET_USER_EMAIL} in env against user, which is
// re-validated here (Sanitized) so no caller can pass an unchecked value
// through. A key whose value names a reserved user variable it cannot
// substitute — every such key for the zero identity, and any key carrying a
// non-exact spelling or FLEET_USER_NAME — is DROPPED, so the server sees the
// variable as unset: never a literal token and never a half-substituted value.
// A map with no user token is returned as-is (no copy).
func ExpandUserEnv(env map[string]string, user MCPUserIdentity) map[string]string {
	if !EnvReferencesUser(env) {
		return env
	}
	user = user.Sanitized()
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !usertoken.Mentions(v) {
			out[k] = v
			continue
		}
		if _, bad := usertoken.FirstInexact(v); bad {
			// An escaped or non-canonical spelling, or the name variable
			// that has no token — the spawn-time backstop for a value that
			// reached the binder some other way than the loader, which
			// refuses them all (internal/clientconfig/user_tokens.go):
			// substituting would hand the connector PII it asked to keep
			// literal, and passing it through would ship a placeholder.
			// Drop the key.
			continue
		}
		if user.Email == "" {
			continue
		}
		// One pass, so a substituted value is never rescanned for a token.
		out[k] = strings.ReplaceAll(v, UserEmailEnvToken, user.Email)
	}
	return out
}
