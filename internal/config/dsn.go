package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// DSN sources ResolveDSNs reports in DSNs.From: the env key that decided the
// DSN, the DB_* parts, or the pure localhost defaults.
const (
	DSNFromDBParts  = "DB_*"
	DSNFromDefaults = "defaults"
)

// The per-store schemas of the one database (#2049, ADR-0098), and Postgres'
// default schema (where a DSN without a schema lands).
const (
	ChatSchema   = "chat"
	SchedSchema  = "sched"
	PublicSchema = "public"
	// IdentitySchema holds the one identity table, core.users, and its side
	// tables (#2049 M4, ADR-0105). The chat store's migrations create it; both
	// stores read it with schema-qualified names, so it is never on a
	// search_path.
	IdentitySchema = "core"
)

// RetiredDSNKeys are the per-store DSN keys of the two-database layout that
// #2049 removed (ADR-0098). fleet no longer reads them for a DSN: one that is
// still set is refused (ResolveDSNs), so a stale env file fails loudly
// instead of silently pointing fleet at another database.
var RetiredDSNKeys = []string{"FLEET_CHAT_DATABASE_URL", "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL"}

// DSNs is the resolved chat + sched Postgres DSN pair: the ONE configured
// database (URL form when built from the DB_* parts), each store's DSN with
// its schema as the connection's only search_path (WithSchema), so every
// consumer that connects with them — the stores, the admin CLI's raw
// queries, psql in the shell helpers — lands in the right schema without
// deciding anything itself. From names what decided the DSN: an env key
// ("DATABASE_URL"), DSNFromDBParts, or DSNFromDefaults when nothing at all
// was configured.
type DSNs struct {
	Chat, Sched string
	From        string
}

// Configured reports whether the DSN came from configuration rather than
// the bare localhost defaults.
func (d DSNs) Configured() bool { return d.From != DSNFromDefaults }

// OverrideFor is the DSN a store consumer connects with when an operator
// passed override (a --database-url flag) for the store whose schema is
// schema ("chat" / "sched"): the resolved DSN when override is empty; an
// override that names its own schema (a search_path in its options)
// verbatim — `options=-csearch_path%3Dpublic` addresses public explicitly;
// any other override with the store's schema, because one database with a
// schema per store is the only layout fleet runs.
func (d DSNs) OverrideFor(override, schema string) string {
	override = strings.TrimSpace(override)
	if override == "" {
		if schema == SchedSchema {
			return d.Sched
		}
		return d.Chat
	}
	if DSNSchema(override) != "" {
		return override
	}
	return WithSchema(override, schema)
}

// ResolveDSNs is the ONE place fleet turns the environment into the chat and
// sched Postgres DSNs (#2018, #2049). `fleet serve`, the MCP broker,
// validate-config, eval, the doctor and every admin CLI verb call it, so no
// two of them can disagree about which database — or which schema — a
// deployment points at; scripts/lib/idle.sh's fleet_resolve_dsn mirrors it
// for the shell, and a parity test pins the two.
//
// The configuration is ONE DSN: DATABASE_URL, else the DB_* parts (DB_HOST,
// DB_PORT, DB_USER, DB_PASSWORD, DB_NAME, DB_SSLMODE; defaults
// localhost:5432, user and database fleet, sslmode=disable; URL form with
// the password escaped). Both stores connect to that database, the chat
// store in schema "chat" and the sched store in schema "sched" (WithSchema).
//
// It fails when one of RetiredDSNKeys is set (RetiredDSNKeysError): the
// two-database layout is gone, and a stale per-store key must not be
// ignored silently.
//
// getenv supplies the values: ProcessEnv for the server, the admin CLI's
// env-or-env-file lookup for its verbs. Values are whitespace-trimmed here,
// except DB_PASSWORD, an opaque secret whose surrounding whitespace may be
// part of it; quote stripping is getenv's job.
func ResolveDSNs(getenv func(string) string) (DSNs, error) {
	get := func(k string) string {
		if k == "DB_PASSWORD" {
			return getenv(k)
		}
		return strings.TrimSpace(getenv(k))
	}
	if err := RetiredDSNKeysError(get); err != nil {
		return DSNs{}, err
	}
	dsn, from := firstDSN(get, "DATABASE_URL")
	if dsn == "" {
		dsn, from = dbPartsDSN(get, "fleet", "fleet")
	}
	return DSNs{Chat: WithSchema(dsn, ChatSchema), Sched: WithSchema(dsn, SchedSchema), From: from}, nil
}

// RetiredDSNKeysError is the refusal of a configuration that still sets one
// of RetiredDSNKeys (non-empty after trimming), naming the keys — never
// their values, which carry a password — and DATABASE_URL, the key that
// replaces them. nil when none is set.
func RetiredDSNKeysError(getenv func(string) string) error {
	var set []string
	for _, k := range RetiredDSNKeys {
		if strings.TrimSpace(getenv(k)) != "" {
			set = append(set, k)
		}
	}
	if len(set) == 0 {
		return nil
	}
	keys := strings.Join(set, ", ")
	return fmt.Errorf("%s is no longer supported: fleet keeps both stores in ONE database, configured by DATABASE_URL alone "+
		"(the two-database layout was removed, ADR-0098) — set DATABASE_URL to that database and delete %s "+
		"from the environment and the env file (docs/ONE-DATABASE.md)", keys, keys)
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
	if path.IsAbs(host) {
		// An absolute path is a Unix-socket directory, which has no place in
		// a URL authority; the drivers take it (and the port, which names the
		// socket file) as query parameters on an empty host.
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

// DSNIdentity returns "<host>:<port>/<dbname>" for the database a DSN
// connects to — its ConnTarget, read exactly as pgx reads it (query
// overrides, PG* environment, defaults) — normalised so that spellings of
// one database compare equal: every local form — a Unix-socket directory,
// localhost, a loopback address — is "local" (one box, one cluster per
// port); other hosts are lower-cased; the database defaults to the role.
// It is "" for a DSN pgx cannot parse, an empty one, or one listing several
// hosts (ErrMultiHost).
func DSNIdentity(dsn string) string {
	t, err := ParseConnTarget(dsn)
	if err != nil || t.DB() == "" {
		return ""
	}
	return identityHost(t.Host) + ":" + t.Port + "/" + t.DB()
}

// identityHost folds every spelling of "this box" into "local".
func identityHost(h string) string {
	if h == "" || strings.HasPrefix(h, "/") || strings.EqualFold(h, "localhost") {
		return "local"
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return "local"
	}
	return strings.ToLower(h)
}

// sameDatabase reports whether two DSNs name the same database: by
// DSNIdentity (so DSNs differing only in user, password, options, schema,
// form or the spelling of a local host still match), and any two DSNs
// identical but for a WithSchema switch always.
func sameDatabase(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if ia := DSNIdentity(a); ia != "" && ia == DSNIdentity(b) {
		return true
	}
	return stripSchemaOption(a) == stripSchemaOption(b)
}

// EnsureDistinctStores fails when the chat and sched DSNs would put both
// stores' tables in the SAME schema of the SAME database. The two stores
// have structurally incompatible `users` tables and `schema_migrations`
// tables of different shapes, so one schema shared by both corrupts startup.
// ResolveDSNs always gives each store its own schema, so this only fires
// for a pair assembled some other way — two `fleet import` flags naming one
// schema, or a hand-built Config (boot, the broker and validate-config keep
// it as a cheap backstop). The error names host:port/db and
// the schema, never the DSN itself, which can carry the password.
func EnsureDistinctStores(chatDSN, schedDSN string) error {
	chatDSN, schedDSN = strings.TrimSpace(chatDSN), strings.TrimSpace(schedDSN)
	if !sameDatabase(chatDSN, schedDSN) {
		return nil
	}
	cs, ss := orDefault(DSNSchema(chatDSN), PublicSchema), orDefault(DSNSchema(schedDSN), PublicSchema)
	if cs != ss {
		return nil
	}
	same := "the same DSN"
	if c := DSNIdentity(chatDSN); c != "" {
		same = fmt.Sprintf("%q", c)
	}
	return fmt.Errorf("chat and sched would share schema %q of one database (%s); each store needs its own schema — "+
		"set only DATABASE_URL and fleet puts chat in schema %q and sched in schema %q (docs/ONE-DATABASE.md)",
		cs, same, ChatSchema, SchedSchema)
}

// schemaOptionPrefix is the startup option WithSchema adds: a -c
// search_path=<schema> run-time parameter in the connection's `options`, so
// the schema is set before the first query on every connection. pgx and
// libpq (psql, pg_dump) both honour `options`; a bare search_path parameter
// would work for pgx only, and psql rejects it.
const schemaOptionPrefix = "-csearch_path="

// WithSchema returns dsn with schema as its connections' only search_path.
// A URL DSN carries it in the `options` query parameter (after any options
// already there), a key=value DSN in an options keyword. Every object the
// store creates then lands in schema, and every unqualified name resolves
// there or in pg_catalog, which Postgres always searches first (so
// gen_random_uuid and every built-in stay visible). schema must be a plain
// lower-case identifier; the stores' migration runners create it
// (internal/dbschema.Ensure).
//
// A URL DSN's query is edited in place — only the options parameter is
// touched, the rest keeps its order and encoding — so the shell mirror
// (fleet_dsn_with_schema in scripts/lib/idle.sh) produces the same bytes.
func WithSchema(dsn, schema string) string {
	dsn = strings.TrimSpace(dsn)
	opt := schemaOptionPrefix + schema
	appendOpt := func(prev string) string {
		if prev = stripSchemaSwitches(prev); prev != "" {
			return prev + " " + opt
		}
		return opt
	}
	// A top-level search_path parameter (pgx accepts one; libpq rejects it)
	// would compete with the options switch — and pgx lets it win — so it is
	// removed: the store's schema must be the connection's only search_path.
	if u, err := url.Parse(dsn); err == nil && isPostgresScheme(u.Scheme) {
		// appendOpt drops an earlier schema switch itself, so the options
		// parameter keeps its place.
		return withRawQuery(dsn, func(q string) string {
			return editRawOption(dropRawParam(q, "search_path"), appendOpt)
		})
	}
	kv, ok := parseKeywordDSN(dsn)
	if !ok {
		// Not a DSN this can rewrite safely; the driver will reject it.
		return dsn
	}
	// In place, after the options already there (a later -c wins).
	return editKeywordOption(dsn, kv, appendOpt, "search_path")
}

// dropRawParam removes every key=value pair named key from a raw URL query,
// keeping the others' order and encoding.
func dropRawParam(rawQuery, key string) string {
	if rawQuery == "" {
		return ""
	}
	var keep []string
	for _, p := range strings.Split(rawQuery, "&") {
		if k, _, _ := strings.Cut(p, "="); k != key {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "&")
}

// DSNSchema returns the schema dsn sets as its search_path, or "" when it
// names none — the database's default search path, i.e. public. A top-level
// search_path parameter (pgx only) wins, as in Postgres, which applies
// startup parameters after the options switches; otherwise the last
// -c search_path=… switch in options counts. Only one plain schema counts; a
// list reports "".
func DSNSchema(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	var options, searchPath string
	if u, err := url.Parse(dsn); err == nil && isPostgresScheme(u.Scheme) {
		options, searchPath = u.Query().Get("options"), u.Query().Get("search_path")
	} else if kv, ok := parseKeywordDSN(dsn); ok {
		options, searchPath = kv.get("options"), kv.get("search_path")
	}
	schema := ""
	fields := strings.Fields(options)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "-c" && i+1 < len(fields) {
			i++
			f = "-c" + fields[i]
		}
		for _, p := range []string{"-csearch_path=", "--search_path=", "--search-path="} {
			if strings.HasPrefix(f, p) {
				schema = strings.TrimPrefix(f, p)
			}
		}
	}
	if searchPath != "" {
		// pgx sends a top-level search_path after options, so it wins.
		schema = searchPath
	}
	if !PlainIdentifier(schema) {
		return ""
	}
	return schema
}

func isPostgresScheme(s string) bool { return s == "postgres" || s == "postgresql" }

// stripSchemaOption removes WithSchema's switch from a DSN's options, so
// sameDatabase compares the DSNs as configured.
func stripSchemaOption(dsn string) string {
	if !strings.Contains(dsn, "search_path") {
		return dsn
	}
	if u, err := url.Parse(dsn); err == nil && isPostgresScheme(u.Scheme) {
		if !strings.Contains(u.Query().Get("options"), schemaOptionPrefix) {
			return dsn
		}
		return withRawQuery(dsn, func(q string) string { return editRawOption(q, stripSchemaSwitches) })
	}
	kv, ok := parseKeywordDSN(dsn)
	if !ok || !strings.Contains(kv.get("options"), schemaOptionPrefix) {
		return dsn
	}
	return editKeywordOption(dsn, kv, stripSchemaSwitches)
}

// withRawQuery applies edit to a URL DSN's raw query text, leaving every
// other byte of the DSN as written (a fragment is kept after the query).
func withRawQuery(dsn string, edit func(string) string) string {
	return withRawQueryImpl(dsn, edit)
}

// secretDSNParams are the connection parameters that carry a credential or
// point at one, in a URL query or a key=value DSN.
var secretDSNParams = map[string]bool{"password": true, "sslpassword": true, "passfile": true}

// SecretDSNParam reports whether a connection parameter name carries (or
// points at) a credential: password, sslpassword, passfile.
func SecretDSNParam(name string) bool { return secretDSNParams[name] }

// RedactDSN is the ONE redaction for a DSN or URL shown to a person (`fleet
// status`, `diagnose`, `backup` errors, migration plans, `fleet config
// explain`):
//
//   - a URL's userinfo becomes "***" — any scheme, not only postgres (a
//     Sentry DSN's key, a webhook's user:password);
//   - in its query, every secret parameter becomes key=***: the pgx/libpq
//     ones (SecretDSNParam: password, sslpassword, passfile, raw key) and any
//     credential-shaped key (token, secret, key, signature, auth, …) matched
//     on the percent-DECODED name (`%74oken` is token), printed as written;
//   - a key=value DSN's secret pairs become key=*** in place.
//
// Everything else is kept, so a host, port, database and options stay
// visible. A string that is neither form is returned unchanged unless it
// mentions "password", which then hides it whole.
func RedactDSN(dsn string) string {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return dsn
	}
	if u, err := url.Parse(trimmed); err == nil && u.Scheme != "" && strings.Contains(trimmed, "://") {
		out := trimmed
		if i := strings.Index(out, "://"); i >= 0 {
			rest := out[i+3:]
			end := len(rest)
			if j := strings.IndexAny(rest, "/?#"); j >= 0 {
				end = j
			}
			if at := strings.LastIndex(rest[:end], "@"); at >= 0 {
				out = out[:i+3] + "***@" + rest[at+1:]
			}
		}
		return withRawQuery(out, func(q string) string {
			if q == "" {
				return q
			}
			pairs := strings.Split(q, "&")
			for i, p := range pairs {
				k, _, hasValue := strings.Cut(p, "=")
				if secretDSNParams[k] || (hasValue && credentialQueryKey(k)) {
					pairs[i] = k + "=***"
				}
			}
			return strings.Join(pairs, "&")
		})
	}
	if kv, ok := parseKeywordDSN(trimmed); ok {
		var b strings.Builder
		last := 0
		for i, k := range kv.keys {
			start, end := kv.spans[i][0], kv.spans[i][1]
			b.WriteString(trimmed[last:start])
			if secretDSNParams[k] {
				b.WriteString(k + "=***")
			} else {
				b.WriteString(trimmed[start:end])
			}
			last = end
		}
		b.WriteString(trimmed[last:])
		return b.String()
	}
	if strings.Contains(strings.ToLower(trimmed), "password") {
		return "[REDACTED]"
	}
	return dsn
}

func withRawQueryImpl(dsn string, edit func(string) string) string {
	base, frag, hasFrag := strings.Cut(dsn, "#")
	base, q, _ := strings.Cut(base, "?")
	out := base
	if q = edit(q); q != "" {
		out += "?" + q
	}
	if hasFrag {
		out += "#" + frag
	}
	return out
}

// editRawOption rewrites the options parameter of a raw URL query in place:
// edit gets its decoded value ("" when absent) and returns the new one; an
// empty result removes it, a new one is appended last. Every other parameter
// keeps its position and its original encoding.
func editRawOption(rawQuery string, edit func(string) string) string {
	var pairs []string
	if rawQuery != "" {
		pairs = strings.Split(rawQuery, "&")
	}
	out := pairs[:0:0]
	prev, at := "", -1
	for _, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		if k == "options" {
			if dv, err := url.QueryUnescape(v); err == nil {
				prev = dv
			}
			if at < 0 {
				at = len(out)
				out = append(out, "")
			}
			continue
		}
		out = append(out, p)
	}
	next := edit(prev)
	switch {
	case next == "" && at >= 0:
		out = append(out[:at], out[at+1:]...)
	case next == "":
	case at >= 0:
		out[at] = "options=" + url.QueryEscape(next)
	default:
		out = append(out, "options="+url.QueryEscape(next))
	}
	return strings.Join(out, "&")
}

// PlainIdentifier reports whether s is a schema or database name fleet uses
// unquoted: a lower-case SQL identifier of at most 63 bytes.
func PlainIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// keywordDSN is a parsed libpq key=value connection string: each pair's key,
// decoded value, and the byte span [start, end) of the whole pair in the
// original text, so a rewrite can change one pair in place.
type keywordDSN struct {
	keys, vals []string
	spans      [][2]int
}

// parseKeywordDSN parses libpq's key=value form: whitespace-separated
// keyword = value pairs, a value bare or single-quoted with \' and \\
// escapes. ok is false for anything else (a URL, a malformed string).
func parseKeywordDSN(s string) (keywordDSN, bool) {
	var kv keywordDSN
	i, n := 0, len(s)
	skipSpace := func() {
		for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
	}
	for {
		skipSpace()
		if i >= n {
			break
		}
		start := i
		for i < n && s[i] != '=' && s[i] != ' ' && s[i] != '\t' {
			i++
		}
		key := s[start:i]
		skipSpace()
		if key == "" || i >= n || s[i] != '=' {
			return keywordDSN{}, false
		}
		i++
		skipSpace()
		var val strings.Builder
		if i < n && s[i] == '\'' {
			i++
			closed := false
			for i < n {
				c := s[i]
				if c == '\\' && i+1 < n {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					i++
					closed = true
					break
				}
				val.WriteByte(c)
				i++
			}
			if !closed {
				return keywordDSN{}, false
			}
		} else {
			for i < n && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
				if s[i] == '\\' && i+1 < n {
					i++
				}
				val.WriteByte(s[i])
				i++
			}
		}
		kv.keys = append(kv.keys, key)
		kv.vals = append(kv.vals, val.String())
		kv.spans = append(kv.spans, [2]int{start, i})
	}
	if len(kv.keys) == 0 {
		return keywordDSN{}, false
	}
	return kv, true
}

func (kv *keywordDSN) get(key string) string {
	v := ""
	for i, k := range kv.keys {
		if k == key {
			v = kv.vals[i] // the last occurrence wins, as in libpq
		}
	}
	return v
}

// editKeywordOption rewrites the options pair of key=value DSN s in place:
// edit gets the decoded value of the LAST options pair (the one libpq uses;
// "" when absent) and returns the new value. The pair is replaced where it
// stands, appended after a space when absent, or removed (with the
// whitespace before it) when edit returns "". Every other byte is kept, so
// the shell mirror (fleet_dsn_with_schema) produces the same text.
//
// Pairs whose key is in drop are removed, each with the whitespace before it.
// The result never starts with whitespace. scripts/lib/idle.sh's
// fleet_dsn_with_schema implements exactly this walk.
func editKeywordOption(s string, kv keywordDSN, edit func(string) string, drop ...string) string {
	dropped := map[string]bool{}
	for _, k := range drop {
		dropped[k] = true
	}
	at := -1
	for i, k := range kv.keys {
		if k == "options" {
			at = i
		}
	}
	prev := ""
	if at >= 0 {
		prev = kv.vals[at]
	}
	next := edit(prev)
	var b strings.Builder
	last := 0
	for i, k := range kv.keys {
		start, end := kv.spans[i][0], kv.spans[i][1]
		gap := s[last:start]
		last = end
		switch {
		case dropped[k], i == at && next == "":
			continue // the pair goes, with the whitespace before it
		case i == at:
			b.WriteString(gap + "options=" + quoteKeywordValue(next))
		default:
			b.WriteString(gap + s[start:end])
		}
	}
	b.WriteString(s[last:])
	if at < 0 && next != "" {
		b.WriteString(" options=" + quoteKeywordValue(next))
	}
	return strings.TrimLeft(b.String(), " \t\n\r")
}

// quoteKeywordValue renders one key=value value, single-quoted (with \ and '
// escaped) only when it is empty or holds whitespace, a quote or a backslash.
func quoteKeywordValue(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\n\r'\\") {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
	}
	return v
}

// stripSchemaSwitches drops WithSchema's switches from an options value.
func stripSchemaSwitches(opts string) string {
	var keep []string
	for _, f := range strings.Fields(opts) {
		if !strings.HasPrefix(f, schemaOptionPrefix) {
			keep = append(keep, f)
		}
	}
	return strings.Join(keep, " ")
}
