package logging

import (
	"net/url"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// registerConfigSecrets registers the loaded value of every setting the
// configuration registry marks Secret, under every spelling (canonical,
// alias, legacy chain), whatever its name looks like: DATABASE_URL names no
// credential, yet carries one (#2074 review). For a DSN- or URL-valued
// secret the credential parts are registered too (dsnCredentials), so a
// message quoting only the password, or the DSN re-encoded, is still caught.
//
// seedEnvLiterals calls it, so it runs wherever the env seeding does: before
// the startup replay, before Fatal's line, and in Configure.
func registerConfigSecrets() {
	for _, e := range envreg.All() {
		if !e.Secret {
			continue
		}
		for _, name := range e.Spellings() {
			v, ok := envreg.Lookup(name)
			if !ok || strings.TrimSpace(v) == "" {
				continue
			}
			values := append([]string{v}, dsnCredentials(v)...)
			RegisterSecretLiterals("", false, values...)
		}
	}
}

// dsnCredentials returns the credential parts of a DSN or URL: a URL's
// userinfo and password (as written and percent-decoded), the secret query
// parameters (password, sslpassword), and a key=value DSN's password values.
// It reads the string by hand, not with url.Parse: an INVALID DSN (a bad
// port, a stray character) is exactly the one a fatal error quotes.
func dsnCredentials(dsn string) []string {
	s := strings.TrimSpace(dsn)
	var out []string
	add := func(v string) {
		if v == "" {
			return
		}
		out = append(out, v)
		if dec, err := url.PathUnescape(v); err == nil && dec != v {
			out = append(out, dec)
		}
	}
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		end := len(rest)
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			end = j
		}
		if at := strings.LastIndex(rest[:end], "@"); at >= 0 {
			userinfo := rest[:at]
			add(userinfo)
			if _, pw, ok := strings.Cut(userinfo, ":"); ok {
				add(pw)
			}
		}
		if _, q, ok := strings.Cut(s, "?"); ok {
			q, _, _ = strings.Cut(q, "#")
			for _, pair := range strings.Split(q, "&") {
				k, v, _ := strings.Cut(pair, "=")
				if secretDSNKey(k) {
					add(v)
					if dec, err := url.QueryUnescape(v); err == nil {
						add(dec)
					}
				}
			}
		}
		return out
	}
	for _, kv := range keywordDSNPairs(s) {
		if secretDSNKey(kv[0]) {
			add(kv[1])
		}
	}
	return out
}

// secretDSNKey names the libpq/pgx connection parameters that carry a
// credential.
func secretDSNKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	return k == "password" || k == "sslpassword"
}

// keywordDSNPairs splits a libpq key=value DSN ("host=db password='a b'")
// into its pairs, unquoting single-quoted values (backslash escapes). A
// malformed tail is read as far as it goes.
func keywordDSNPairs(s string) [][2]string {
	var out [][2]string
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
			i++
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq <= 0 {
			break
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		for i < len(s) && s[i] == ' ' {
			i++
		}
		var val strings.Builder
		if i < len(s) && s[i] == '\'' {
			i++
			for i < len(s) && s[i] != '\'' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				val.WriteByte(s[i])
				i++
			}
			i++ // closing quote
		} else {
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				val.WriteByte(s[i])
				i++
			}
		}
		out = append(out, [2]string{key, val.String()})
	}
	return out
}
