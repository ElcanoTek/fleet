package config

import (
	"net/url"
	"regexp"
	"strings"
)

// RedactDSN strips the credentials from a connection string or URL so it can
// be shown to an operator: the userinfo of a URL (`user:password@` becomes
// `***@`), the value of every query parameter whose name looks like a
// credential (token, key, secret, password, signature, auth, …), and the
// password of a key=value DSN (`password=…`). Everything else — scheme, host,
// port, path, database — stays readable. It is the one helper behind `fleet
// status`, `fleet diagnose`, `fleet backup` errors and `fleet config explain`.
func RedactDSN(v string) string {
	if i := strings.Index(v, "://"); i >= 0 {
		scheme, rest := v[:i+3], v[i+3:]
		// userinfo ends at the LAST '@' before the path/query/fragment.
		end := len(rest)
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			end = j
		}
		if at := strings.LastIndexByte(rest[:end], '@'); at >= 0 {
			rest = "***@" + rest[at+1:]
		}
		return scheme + redactQuery(rest)
	}
	return kvPassword.ReplaceAllString(v, "${1}=***")
}

// kvPassword matches the password of a key=value (libpq) DSN.
var kvPassword = regexp.MustCompile(`(?i)\b((?:ssl)?password)\s*=\s*('[^']*'|\S+)`)

// credentialParam reports whether a query parameter name looks like it holds
// a credential.
func credentialParam(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"token", "secret", "password", "passwd", "pwd", "key", "sig", "auth", "credential"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// redactQuery replaces credential-looking query values in rest (everything
// after the scheme), keeping parameter order and every other byte.
func redactQuery(rest string) string {
	q := strings.IndexByte(rest, '?')
	if q < 0 {
		return rest
	}
	query, frag := rest[q+1:], ""
	if h := strings.IndexByte(query, '#'); h >= 0 {
		query, frag = query[:h], query[h:]
	}
	parts := strings.Split(query, "&")
	for i, p := range parts {
		name, _, hasValue := strings.Cut(p, "=")
		// Match the DECODED name (`%74oken` is `token`), keep the original
		// spelling in the output. An undecodable name is matched as written.
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		if hasValue && credentialParam(decoded) {
			parts[i] = name + "=REDACTED"
		}
	}
	return rest[:q+1] + strings.Join(parts, "&") + frag
}
