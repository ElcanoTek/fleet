package config

import (
	"net/url"
	"strings"
)

// credentialQueryKey reports whether a raw URL query key names a credential,
// matched on its percent-DECODED form (`%74oken` is token); an undecodable
// key is matched as written. RedactDSN (dsn.go) uses it next to the pgx/libpq
// secret parameters.
func credentialQueryKey(raw string) bool {
	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		decoded = raw
	}
	return credentialParam(decoded)
}

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
