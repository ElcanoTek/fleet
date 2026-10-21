// Package usertoken recognizes every spelling that NAMES one of fleet's
// reserved signed-in-user env tokens, ${FLEET_USER_EMAIL} and
// ${FLEET_USER_NAME} (ADR-0087, docs/MCP-BUNDLE-ENV.md).
//
// The spawn paths substitute only the two exact spellings. Anything else that
// names a token — an escape ("$${FLEET_USER_EMAIL}"), padding of ANY kind
// inside the braces (ASCII or Unicode whitespace, a zero-width space, a BOM,
// stray punctuation), or a colon operator ("${FLEET_USER_NAME:-team}") — would
// either collapse into the exact token one pass too early (handing a connector
// the user's PII it asked to keep literal) or reach a connector as a literal
// placeholder. The bundle loader (internal/clientconfig) refuses those
// spellings and the binder (internal/agentcore) drops any that slip through;
// both use this one matcher so the two sides can never disagree on what
// "names a token" means.
package usertoken

import (
	"regexp"
	"strings"
)

const (
	// EmailVar and NameVar are the reserved token names.
	EmailVar = "FLEET_USER_EMAIL"
	NameVar  = "FLEET_USER_NAME"
	// EmailToken and NameToken are the only spellings fleet substitutes.
	EmailToken = "${" + EmailVar + "}"
	NameToken  = "${" + NameVar + "}"
)

// spelling matches, after an optional escape "$": "${", any run of
// non-identifier characters (which is how padding of every kind is caught —
// RE2's \s is ASCII-only), the token name, then either "}" or a
// non-identifier character and anything up to the closing brace. The
// identifier boundaries keep ${FLEET_USER_NAMES} and ${MY_FLEET_USER_EMAIL}
// out; '{', '}' and '$' are excluded from the prefix so a match always starts
// at the "${" that actually opens the token.
var spelling = regexp.MustCompile(`(\$?)\$\{([^{}$A-Za-z0-9_]*)(` + EmailVar + `|` + NameVar + `)((?:[^}A-Za-z0-9_][^}]*)?)\}`)

// Spelling is one occurrence of a token name inside "${...}".
type Spelling struct {
	Text    string // the matched text, escape included
	Var     string // EmailVar or NameVar
	Escaped bool   // preceded by the "$" of a "$${" escape
	// Exact reports the unescaped canonical spelling — the only one the spawn
	// paths substitute.
	Exact bool
}

// Find returns every spelling in s that names a user token, in order.
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
			Exact:   !escaped && m[2] == "" && m[4] == "",
		})
	}
	return out
}

// Mentions reports whether s names a user token in any spelling.
func Mentions(s string) bool {
	return strings.Contains(s, "FLEET_USER_") && spelling.MatchString(s)
}

// FirstInexact returns the first spelling in s that is NOT the exact,
// unescaped token, and false when every spelling present (if any) is exact.
func FirstInexact(s string) (Spelling, bool) {
	for _, sp := range Find(s) {
		if !sp.Exact {
			return sp, true
		}
	}
	return Spelling{}, false
}
