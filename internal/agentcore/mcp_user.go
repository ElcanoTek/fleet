package agentcore

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reserved ${FLEET_USER_EMAIL} / ${FLEET_USER_NAME} manifest-env tokens
// (ADR-0087, docs/MCP-BUNDLE-ENV.md §1b).
//
// A bundle connector that writes something addressed to a person — a deal-sheet
// email that greets the trader and defaults the recipient to them — needs to
// know who that person is. Nothing else in a server's env can tell it: the
// operator's process env is deployment-wide, and the model's arguments are
// model-authored text, which is exactly the wrong place to learn whose turn
// this is. These two tokens let a bundle ask for it in an mcp_servers[].env
// value (stdio servers only, like the workspace tokens):
//
//	env:
//	  DEAL_SHEET_DEFAULT_TO: "${FLEET_USER_EMAIL}"
//	  DEAL_SHEET_GREETING_NAME: "${FLEET_USER_NAME}"
//
// The values come from ONE place: the authenticated user who sent an
// interactive chat turn (or, for an approved card, the user who approved it —
// the same person, since an approval is claimable only by its own user). They
// are resolved in the parent from the request's authenticated identity, never
// from the conversation, the model, or the bundle, cross to the credential
// owner as ScopeSpec.User, and are substituted only when that child opens the
// turn's scope. Every other spawn path — the boot catalog, hot reload,
// load-on-demand onto a shared client, `fleet mcp test`, and every scheduled
// run, dedicated client or not — has no signed-in user and DROPS each
// token-bearing key, so the server sees the variable as unset. A key is never
// handed a literal token, an empty string, or another user's identity.
//
// The values are data, not instructions, and not authorization: a server must
// never treat them as a credential or a seat selector. Both are validated
// against a conservative character set on BOTH sides of the broker boundary
// (the child does not trust the parent's formatting), and a value that fails
// is treated as absent — dropped, not passed through. Neither set admits '$',
// '{' or '}', so a substituted value can never form another reserved token for
// a later expansion pass to resolve.
const (
	// UserEmailEnvToken resolves to the turn sender's normalized (lowercased)
	// email address.
	//nolint:gosec // G101 false positive: an interpolation placeholder name, not a credential.
	UserEmailEnvToken = "${FLEET_USER_EMAIL}"
	// UserNameEnvToken resolves to the best display name fleet knows for the
	// turn sender. Fleet's auth carries no name claim today, so this is the
	// first name DERIVED from the email's local part (DeriveMCPUserName); when
	// no plausible name can be derived the token is treated as absent.
	UserNameEnvToken = "${FLEET_USER_NAME}" //nolint:gosec // interpolation placeholder, not a credential.

	// maxMCPUserEmailLen is RFC 5321's path limit; maxMCPUserNameLen bounds a
	// display name in runes.
	maxMCPUserEmailLen = 254
	maxMCPUserNameLen  = 64
)

// MCPUserIdentity is the signed-in user an interactive MCP scope is opened
// for. The zero value means "no user" — the only value any non-interactive
// spawn path may use.
type MCPUserIdentity struct {
	Email string
	Name  string
}

// IsZero reports whether the identity carries nothing usable.
func (u MCPUserIdentity) IsZero() bool { return u.Email == "" && u.Name == "" }

// NewMCPUserIdentity builds the identity for an authenticated user's email:
// the normalized email (when it passes ValidMCPUserEmail) plus the derived
// display name. An email that fails validation yields the zero identity, so a
// malformed value can never reach a connector.
func NewMCPUserIdentity(email string) MCPUserIdentity {
	email = strings.ToLower(strings.TrimSpace(email))
	if !ValidMCPUserEmail(email) {
		return MCPUserIdentity{}
	}
	return MCPUserIdentity{Email: email, Name: DeriveMCPUserName(email)}
}

// Sanitized returns u with every field that fails validation cleared. The
// credential owner calls it on the identity it receives over the wire.
func (u MCPUserIdentity) Sanitized() MCPUserIdentity {
	out := MCPUserIdentity{}
	if email := strings.ToLower(strings.TrimSpace(u.Email)); ValidMCPUserEmail(email) {
		out.Email = email
	}
	if name := strings.TrimSpace(u.Name); ValidMCPUserName(name) {
		out.Name = name
	}
	return out
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

// ValidMCPUserName reports whether s is a display name fleet will hand to a
// connector: 1–64 runes of letters, single spaces, and . ' - — no digits,
// punctuation that could carry markup or a token, or control characters.
func ValidMCPUserName(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxMCPUserNameLen {
		return false
	}
	if strings.TrimSpace(s) != s || strings.Contains(s, "  ") {
		return false
	}
	hasLetter := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case r == ' ', r == '.', r == '\'', r == '-':
		default:
			return false
		}
	}
	return hasLetter
}

// DeriveMCPUserName guesses a first name from an email's local part: the text
// before any '+' tag, cut at the first '.', '_' or '-', and capitalized —
// "elyse@x" and "elyse.messer@x" both give "Elyse". It returns "" (the token is
// then treated as absent) unless that segment is 2–64 ASCII letters, so
// "j.smith", "ops2" and "x" give nothing rather than a wrong greeting. It is a
// heuristic: "jsmith@x" gives "Jsmith". Fleet's sign-in carries an email and
// no name claim; a real name source, when one exists, replaces this function
// without any bundle change.
func DeriveMCPUserName(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return ""
	}
	local := email[:at]
	if i := strings.IndexByte(local, '+'); i >= 0 {
		local = local[:i]
	}
	if i := strings.IndexAny(local, "._-"); i >= 0 {
		local = local[:i]
	}
	if len(local) < 2 || len(local) > maxMCPUserNameLen {
		return ""
	}
	for i := 0; i < len(local); i++ {
		c := local[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return ""
		}
	}
	return strings.ToUpper(local[:1]) + strings.ToLower(local[1:])
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// EnvReferencesUser reports whether any value in env carries a reserved user
// token.
func EnvReferencesUser(env map[string]string) bool {
	for _, v := range env {
		if strings.Contains(v, UserEmailEnvToken) || strings.Contains(v, UserNameEnvToken) {
			return true
		}
	}
	return false
}

// ExpandUserEnv resolves the reserved user tokens in env against user, which
// is re-validated here (Sanitized) so no caller can pass an unchecked value
// through. A key whose value references a token with no valid value — every
// key, for the zero identity — is DROPPED, so the server sees the variable as
// unset: never a literal token, never a half-substituted value such as
// "Hi ${FLEET_USER_NAME}" with only the email known. A map with no user token
// is returned as-is (no copy).
func ExpandUserEnv(env map[string]string, user MCPUserIdentity) map[string]string {
	if !EnvReferencesUser(env) {
		return env
	}
	user = user.Sanitized()
	// One pass, so a substituted value is never rescanned for a token.
	replacer := strings.NewReplacer(UserEmailEnvToken, user.Email, UserNameEnvToken, user.Name)
	out := make(map[string]string, len(env))
	for k, v := range env {
		usesEmail := strings.Contains(v, UserEmailEnvToken)
		usesName := strings.Contains(v, UserNameEnvToken)
		if !usesEmail && !usesName {
			out[k] = v
			continue
		}
		if (usesEmail && user.Email == "") || (usesName && user.Name == "") {
			continue
		}
		out[k] = replacer.Replace(v)
	}
	return out
}
