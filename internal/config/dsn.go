package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// DSN sources ResolveDSNs reports in DSNs.ChatFrom / DSNs.SchedFrom: the env
// key that decided the DSN, the DB_* parts, or the pure localhost defaults.
const (
	DSNFromDBParts  = "DB_*"
	DSNFromDefaults = "defaults"
)

// DSNs is the resolved chat + sched Postgres DSN pair, both in URL form.
// ChatFrom / SchedFrom name what decided each one: an env key
// ("FLEET_CHAT_DATABASE_URL", "DATABASE_URL", …), DSNFromDBParts, or
// DSNFromDefaults when nothing at all was configured.
type DSNs struct {
	Chat, Sched         string
	ChatFrom, SchedFrom string
}

// ChatConfigured reports whether the chat DSN came from configuration rather
// than the bare localhost defaults.
func (d DSNs) ChatConfigured() bool { return d.ChatFrom != DSNFromDefaults }

// SchedConfigured reports whether the sched DSN came from configuration rather
// than the bare localhost defaults.
func (d DSNs) SchedConfigured() bool { return d.SchedFrom != DSNFromDefaults }

// ChatFromGeneric reports whether the chat DSN reached the generic fallbacks
// (DATABASE_URL, DB_* parts, defaults) shared with the sched resolver, rather
// than a chat-specific key.
func (d DSNs) ChatFromGeneric() bool { return d.ChatFrom != "FLEET_CHAT_DATABASE_URL" }

// SchedFromGeneric is ChatFromGeneric for the sched DSN.
func (d DSNs) SchedFromGeneric() bool {
	return d.SchedFrom != "FLEET_SCHED_DATABASE_URL" && d.SchedFrom != "SCHED_DATABASE_URL"
}

// ResolveDSNs is the ONE place fleet turns the environment into the chat and
// sched Postgres DSNs (#2018). `fleet serve`, the MCP broker, validate-config,
// eval, the doctor and every admin CLI verb call it, so no two of them can
// disagree about which database a deployment points at. Precedence:
//
//	chat:  FLEET_CHAT_DATABASE_URL, else DATABASE_URL, else the DB_* parts
//	sched: FLEET_SCHED_DATABASE_URL, else SCHED_DATABASE_URL, else
//	       DATABASE_URL, else the DB_* parts
//
// The DB_* parts (DB_HOST, DB_PORT, DB_USER, DB_PASSWORD, DB_NAME,
// DB_SSLMODE) default to localhost:5432, sslmode=disable, and per store
// user/database chat/chat and fleet/sched; with nothing set at all both DSNs
// still resolve, to those defaults (From = DSNFromDefaults). Both results
// are URL form with the password escaped.
//
// getenv supplies the values: ProcessEnv for the server, the admin CLI's
// env-or-env-file lookup for its verbs. Values are whitespace-trimmed here,
// except DB_PASSWORD, an opaque secret whose surrounding whitespace may be
// part of it; quote stripping is getenv's job.
func ResolveDSNs(getenv func(string) string) DSNs {
	get := func(k string) string {
		if k == "DB_PASSWORD" {
			return getenv(k)
		}
		return strings.TrimSpace(getenv(k))
	}
	var d DSNs
	d.Chat, d.ChatFrom = firstDSN(get, "FLEET_CHAT_DATABASE_URL", "DATABASE_URL")
	if d.Chat == "" {
		d.Chat, d.ChatFrom = dbPartsDSN(get, "chat", "chat")
	}
	d.Sched, d.SchedFrom = firstDSN(get, "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL", "DATABASE_URL")
	if d.Sched == "" {
		d.Sched, d.SchedFrom = dbPartsDSN(get, "fleet", "sched")
	}
	return d
}

// ProcessEnv is the getenv ResolveDSNs takes for the server process: the
// process environment with one layer of surrounding quotes stripped, the
// same rule every other config knob follows (docs/OPERATORS.md).
func ProcessEnv(key string) string { return creds.StripEnvQuotes(os.Getenv(key)) }

func firstDSN(get func(string) string, keys ...string) (string, string) {
	for _, k := range keys {
		if v := get(k); v != "" {
			return v, k
		}
	}
	return "", ""
}

// dbPartsDSN builds the URL-form DSN from the DB_* parts, reporting
// DSNFromDefaults when none of them is set.
func dbPartsDSN(get func(string) string, defaultUser, defaultName string) (string, string) {
	host, port, user := get("DB_HOST"), get("DB_PORT"), get("DB_USER")
	pass, name, ssl := get("DB_PASSWORD"), get("DB_NAME"), get("DB_SSLMODE")
	from := DSNFromDBParts
	if host == "" && port == "" && user == "" && pass == "" && name == "" && ssl == "" {
		from = DSNFromDefaults
	}
	host = orDefault(host, "localhost")
	port = orDefault(port, "5432")
	user = orDefault(user, defaultUser)
	name = orDefault(name, defaultName)
	ssl = orDefault(ssl, "disable")
	u := &url.URL{Scheme: "postgres", User: url.User(user), Path: "/" + name}
	if pass != "" {
		u.User = url.UserPassword(user, pass)
	}
	q := url.Values{}
	if strings.HasPrefix(host, "/") {
		// A Unix-socket directory has no place in a URL authority; the
		// drivers take it (and the port, which names the socket file) as
		// query parameters on an empty host.
		q.Set("host", host)
		q.Set("port", port)
	} else {
		// JoinHostPort brackets an IPv6 literal ("[2001:db8::1]:5432").
		u.Host = net.JoinHostPort(host, port)
	}
	q.Set("sslmode", ssl)
	u.RawQuery = q.Encode()
	return u.String(), from
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// DSNIdentity returns host:port/dbname for a postgres URL DSN (a socket host
// given as ?host=…&port=… counts as the host), or "" when the DSN is not a
// parseable URL with a host; key=value DSNs are compared verbatim by
// EnsureDistinctDatabases instead.
func DSNIdentity(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	host := u.Host
	if host == "" {
		if h := u.Query().Get("host"); h != "" {
			host = net.JoinHostPort(h, u.Query().Get("port"))
		}
	}
	if host == "" {
		return ""
	}
	return host + u.Path
}

// EnsureDistinctDatabases fails when the chat and sched DSNs name the SAME
// database. The two stores use structurally incompatible `users` schemas and
// different migration runners, so sharing one database corrupts startup. Pass
// the RESOLVED DSNs (ResolveDSNs) — comparing anything less misses the shared
// DATABASE_URL and the shared-DB_NAME shapes. URL DSNs compare by
// host:port/dbname (so DSNs differing only in user or options still match);
// any two identical DSNs — key=value form included — always match.
func EnsureDistinctDatabases(chatDSN, schedDSN string) error {
	chatDSN, schedDSN = strings.TrimSpace(chatDSN), strings.TrimSpace(schedDSN)
	// The message names host:port/db, never the DSN itself: a key=value DSN
	// can carry the password.
	same := "the same DSN"
	if c := DSNIdentity(chatDSN); c != "" && c == DSNIdentity(schedDSN) {
		same = fmt.Sprintf("%q", c)
	} else if chatDSN == "" || chatDSN != schedDSN {
		return nil
	}
	return fmt.Errorf("chat and sched must use SEPARATE databases (incompatible users schemas + migration runners) but both resolve to %s; set distinct FLEET_CHAT_DATABASE_URL and FLEET_SCHED_DATABASE_URL", same)
}
