package config

import (
	"fmt"
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
// env-or-env-file lookup for its verbs. Values are whitespace-trimmed here;
// quote stripping is getenv's job.
func ResolveDSNs(getenv func(string) string) DSNs {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
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
	auth := url.User(user).String()
	if pass != "" {
		auth = url.UserPassword(user, pass).String()
	}
	return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=%s", auth, host, port, url.PathEscape(name), url.QueryEscape(ssl)), from
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// DSNIdentity returns host:port/dbname for a postgres URL DSN, or "" when the
// DSN is not a parseable URL (a key=value DSN is left to the drivers).
func DSNIdentity(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host + u.Path
}

// EnsureDistinctDatabases fails when the chat and sched DSNs name the SAME
// database. The two stores use structurally incompatible `users` schemas and
// different migration runners, so sharing one database corrupts startup. Pass
// the RESOLVED DSNs (ResolveDSNs) — comparing anything less misses the shared
// DATABASE_URL and the shared-DB_NAME shapes. Best-effort on the URL form; a
// key=value DSN is left to the drivers.
func EnsureDistinctDatabases(chatDSN, schedDSN string) error {
	c := DSNIdentity(chatDSN)
	if c != "" && c == DSNIdentity(schedDSN) {
		return fmt.Errorf("chat and sched must use SEPARATE databases (incompatible users schemas + migration runners) but both resolve to %q; set distinct FLEET_CHAT_DATABASE_URL and FLEET_SCHED_DATABASE_URL", c)
	}
	return nil
}
