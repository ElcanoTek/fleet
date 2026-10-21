// Package usertoken recognizes every spelling that NAMES fleet's reserved
// signed-in-user env token, ${FLEET_USER_EMAIL} (ADR-0087,
// docs/MCP-BUNDLE-ENV.md), and the reserved-but-refused FLEET_USER_NAME.
//
// The spawn paths substitute only the exact ${FLEET_USER_EMAIL}. Anything else
// that names it — an escape ("$${FLEET_USER_EMAIL}"), padding of ANY kind
// inside the braces (ASCII or Unicode whitespace, a zero-width space, a BOM,
// stray punctuation), or a colon operator ("${FLEET_USER_EMAIL:-x}") — would
// either collapse into the exact token one pass too early (handing a connector
// the user's PII it asked to keep literal) or reach a connector as a literal
// placeholder.
//
// FLEET_USER_NAME is reserved with NO token behind it: fleet has no name for
// the signed-in user (sign-in carries an email and no name claim, and a name
// is never guessed from the email), so every spelling of it is refused — an
// operator-exported FLEET_USER_NAME must never hand one fixed person's name to
// every user's turn, and a bundle that writes it must learn at load that it
// will never resolve.
//
// The bundle loader (internal/clientconfig) refuses every non-exact spelling
// and the binder (internal/agentcore) drops any key that still carries one;
// both use this one matcher so the two sides can never disagree on what
// "names a token" means.
package usertoken

import (
	"regexp"
	"strings"
)

const (
	// EmailVar is the reserved signed-in-user email token's name, and
	// EmailToken its only spelling fleet substitutes.
	EmailVar   = "FLEET_USER_EMAIL"
	EmailToken = "${" + EmailVar + "}"
	// ReservedNameVar is reserved so it can never resolve from the process
	// env, but no name token exists: every spelling of it is refused at load
	// and dropped at spawn.
	ReservedNameVar = "FLEET_USER_NAME"
)

// spelling matches, after an optional escape "$": "${", any run of
// non-identifier characters (which is how padding of every kind is caught —
// RE2's \s is ASCII-only), a reserved name, then either "}" or a
// non-identifier character and anything up to the closing brace. The
// identifier boundaries keep ${FLEET_USER_NAMES} and ${MY_FLEET_USER_EMAIL}
// out; '{', '}' and '$' are excluded from the prefix so a match always starts
// at the "${" that actually opens the token.
var spelling = regexp.MustCompile(`(\$?)\$\{([^{}$A-Za-z0-9_]*)(` + EmailVar + `|` + ReservedNameVar + `)((?:[^}A-Za-z0-9_][^}]*)?)\}`)

// Spelling is one occurrence of a reserved name inside "${...}".
type Spelling struct {
	Text    string // the matched text, escape included
	Var     string // EmailVar or ReservedNameVar
	Escaped bool   // preceded by the "$" of a "$${" escape
	// Exact reports the unescaped canonical ${FLEET_USER_EMAIL} — the only
	// spelling the spawn paths substitute. Never true for ReservedNameVar.
	Exact bool
}

// Find returns every spelling in s that names a reserved user variable, in
// order.
func Find(s string) []Spelling {
	if !strings.Contains(s, "FLEET_USER_") {
		return nil
	}
	ms := spelling.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]Spelling, 0, len(ms))
	for _, m := range ms {
		escaped := m[1] != ""
		out = append(out, Spelling{
			Text:    m[0],
			Var:     m[3],
			Escaped: escaped,
			Exact:   m[3] == EmailVar && !escaped && m[2] == "" && m[4] == "",
		})
	}
	return out
}

// Mentions reports whether s names a reserved user variable in any spelling.
func Mentions(s string) bool {
	return strings.Contains(s, "FLEET_USER_") && spelling.MatchString(s)
}

// FirstInexact returns the first spelling in s that is NOT the exact,
// unescaped ${FLEET_USER_EMAIL}, and false when every spelling present (if
// any) is exact.
func FirstInexact(s string) (Spelling, bool) {
	for _, sp := range Find(s) {
		if !sp.Exact {
			return sp, true
		}
	}
	return Spelling{}, false
}
