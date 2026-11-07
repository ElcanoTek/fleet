package config

import (
	"errors"
	"strings"
	"testing"
)

const (
	chatOpt  = "options=-csearch_path%3Dchat"
	schedOpt = "options=-csearch_path%3Dsched"
)

// TestResolveDSNsOneDatabase pins the supported configuration (#2049): one
// DSN, both stores in it, each in its own schema.
func TestResolveDSNsOneDatabase(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		chat, sched string
		from        string
	}{
		{
			name:  "DATABASE_URL alone",
			env:   map[string]string{"DATABASE_URL": "  postgres://u:pw@h:5432/fleet?sslmode=disable  "},
			chat:  "postgres://u:pw@h:5432/fleet?sslmode=disable&" + chatOpt,
			sched: "postgres://u:pw@h:5432/fleet?sslmode=disable&" + schedOpt,
			from:  "DATABASE_URL",
		},
		{
			name:  "DATABASE_URL beats DB_* parts",
			env:   map[string]string{"DATABASE_URL": "postgres://g/db", "DB_HOST": "db.internal", "DB_NAME": "x"},
			chat:  "postgres://g/db?" + chatOpt,
			sched: "postgres://g/db?" + schedOpt,
			from:  "DATABASE_URL",
		},
		{
			name:  "existing options are kept, the schema switch is appended",
			env:   map[string]string{"DATABASE_URL": "postgres://g/db?options=-cstatement_timeout%3D5000"},
			chat:  "postgres://g/db?options=-cstatement_timeout%3D5000+-csearch_path%3Dchat",
			sched: "postgres://g/db?options=-cstatement_timeout%3D5000+-csearch_path%3Dsched",
			from:  "DATABASE_URL",
		},
		{
			name:  "DB_NAME names the one database; DB_USER defaults to fleet",
			env:   map[string]string{"DB_HOST": "db.internal", "DB_PORT": "6543", "DB_PASSWORD": "p@ss #1", "DB_NAME": "one", "DB_SSLMODE": "require"},
			chat:  "postgres://fleet:p%40ss%20%231@db.internal:6543/one?sslmode=require&" + chatOpt,
			sched: "postgres://fleet:p%40ss%20%231@db.internal:6543/one?sslmode=require&" + schedOpt,
			from:  DSNFromDBParts,
		},
		{
			name:  "DB_USER/DB_NAME apply to both stores",
			env:   map[string]string{"DB_USER": "app", "DB_NAME": "one"},
			chat:  "postgres://app@localhost:5432/one?sslmode=disable&" + chatOpt,
			sched: "postgres://app@localhost:5432/one?sslmode=disable&" + schedOpt,
			from:  DSNFromDBParts,
		},
		{
			name:  "a Unix-socket DB_HOST moves host and port into the query",
			env:   map[string]string{"DB_HOST": "/var/run/postgresql", "DB_NAME": "fleet"},
			chat:  "postgres://fleet@/fleet?host=%2Fvar%2Frun%2Fpostgresql&port=5432&sslmode=disable&" + chatOpt,
			sched: "postgres://fleet@/fleet?host=%2Fvar%2Frun%2Fpostgresql&port=5432&sslmode=disable&" + schedOpt,
			from:  DSNFromDBParts,
		},
		{
			name:  "an IPv6 DB_HOST is bracketed in the authority",
			env:   map[string]string{"DB_HOST": "2001:db8::1", "DB_NAME": "fleet"},
			chat:  "postgres://fleet@[2001:db8::1]:5432/fleet?sslmode=disable&" + chatOpt,
			sched: "postgres://fleet@[2001:db8::1]:5432/fleet?sslmode=disable&" + schedOpt,
			from:  DSNFromDBParts,
		},
		{
			name:  "DB_PASSWORD keeps surrounding whitespace (an opaque secret)",
			env:   map[string]string{"DB_HOST": " db ", "DB_PASSWORD": " pw ", "DB_NAME": "fleet"},
			chat:  "postgres://fleet:%20pw%20@db:5432/fleet?sslmode=disable&" + chatOpt,
			sched: "postgres://fleet:%20pw%20@db:5432/fleet?sslmode=disable&" + schedOpt,
			from:  DSNFromDBParts,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ResolveDSNs(func(k string) string { return tc.env[k] })
			if d.Chat != tc.chat || d.Sched != tc.sched {
				t.Errorf("DSNs = (%q, %q), want (%q, %q)", d.Chat, d.Sched, tc.chat, tc.sched)
			}
			if d.ChatFrom != tc.from || d.SchedFrom != tc.from {
				t.Errorf("From = (%q, %q), want %q", d.ChatFrom, d.SchedFrom, tc.from)
			}
			if d.Layout != LayoutSchemas || d.ChatSchema != ChatSchema || d.SchedSchema != SchedSchema {
				t.Errorf("layout = %q (%q, %q), want schemas (chat, sched)", d.Layout, d.ChatSchema, d.SchedSchema)
			}
			if DSNSchema(d.Chat) != ChatSchema || DSNSchema(d.Sched) != SchedSchema {
				t.Errorf("DSNSchema = (%q, %q)", DSNSchema(d.Chat), DSNSchema(d.Sched))
			}
			if err := EnsureDistinctStores(d.Chat, d.Sched); err != nil {
				t.Errorf("EnsureDistinctStores refused the resolved pair: %v", err)
			}
		})
	}
}

func TestWithSchemaAndDSNSchema(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"url", "postgres://u@h/db", "postgres://u@h/db?" + chatOpt},
		{"url is idempotent", "postgres://u@h/db?" + schedOpt, "postgres://u@h/db?" + chatOpt},
		{"keyword form", "host=h dbname=db password=x", "host=h dbname=db password=x options=-csearch_path=chat"},
		{"keyword form keeps quoted options", "host=h options='-c work_mem=4MB'", "host=h options='-c work_mem=4MB -csearch_path=chat'"},
		{"keyword form quoting survives", `host=h password='a b\'c'`, `host=h password='a b\'c' options=-csearch_path=chat`},
		// #2055 review: an existing options pair is extended where it stands,
		// every other byte kept, so the shell mirror can match exactly.
		{"keyword options in the middle stay in place", `host=h options=-cwork_mem=4MB dbname=x`, `host=h options='-cwork_mem=4MB -csearch_path=chat' dbname=x`},
		{"keyword: the last options pair is the one edited", `options=-ca=1 host=h options='-c b=2'`, `options=-ca=1 host=h options='-c b=2 -csearch_path=chat'`},
		{"keyword: a schema switch is replaced, not stacked", `host=h options='-ca=1 -csearch_path=sched'`, `host=h options='-ca=1 -csearch_path=chat'`},
		// #2055 review: a top-level search_path (pgx only) would win over the
		// options switch, so it is removed, in both forms.
		{"url: a top-level search_path is removed", "postgres://u@h/db?search_path=other&sslmode=disable", "postgres://u@h/db?sslmode=disable&" + chatOpt},
		{"url: only search_path params removed", "postgres://u@h/db?search_path=a&options=-ca%3D1&search_path=b", "postgres://u@h/db?options=-ca%3D1+-csearch_path%3Dchat"},
		{"keyword: a leading search_path is removed", "search_path=other host=h dbname=db", "host=h dbname=db options=-csearch_path=chat"},
		{"keyword: a middle search_path is removed", "host=h search_path='a, b' options='-ca=1' dbname=x", "host=h options='-ca=1 -csearch_path=chat' dbname=x"},
		{"url: existing options extended in place", "postgres://u@h/db?options=-ca%3D1&sslmode=disable", "postgres://u@h/db?options=-ca%3D1+-csearch_path%3Dchat&sslmode=disable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WithSchema(tc.in, ChatSchema)
			if got != tc.want {
				t.Fatalf("WithSchema(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if s := DSNSchema(got); s != ChatSchema {
				t.Errorf("DSNSchema(%q) = %q", got, s)
			}
			if !sameDatabase(got, tc.in) {
				t.Errorf("sameDatabase(%q, %q) = false", got, tc.in)
			}
		})
	}
	for dsn, want := range map[string]string{
		"postgres://u@h/db":                                 "",
		"postgres://u@h/db?search_path=x":                   "x",
		"postgres://u@h/db?options=-c%20search_path%3Dy":    "y",
		"postgres://u@h/db?options=-csearch_path%3Da%2Cb":   "",
		"postgres://u@h/db?options=--search_path%3DUpper":   "",
		"host=h options='-c search_path=k'":                 "k",
		"postgres://u@h/db?options=-csearch_path%3D%22x%22": "",
	} {
		if got := DSNSchema(dsn); got != want {
			t.Errorf("DSNSchema(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// TestOverrideFor pins how a --database-url override's layout is decided
// (#2055 review: an override naming another database used to stay in
// public even on a one-database deployment).
func TestOverrideFor(t *testing.T) {
	one := ResolveDSNs(func(k string) string {
		return map[string]string{"DATABASE_URL": "postgres://u:pw@h:5432/fleet"}[k]
	})
	legacy := ResolveDSNs(func(k string) string {
		return map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://c@h:5432/chat", "FLEET_SCHED_DATABASE_URL": "postgres://s@h:5432/sched"}[k]
	})
	publicStore := func(string) (bool, error) { return true, nil }
	empty := func(string) (bool, error) { return false, nil }
	unreachable := func(string) (bool, error) { return false, errors.New("connection refused") }
	const other = "postgres://u@dr-host:5432/fleet"
	cases := []struct {
		name     string
		d        DSNs
		override string
		probe    PublicStoreProbe
		want     string // the schema the result names ("" = verbatim, public)
	}{
		{"no override is the resolved DSN", one, "", nil, ChatSchema},
		{"same database, one-database deployment", one, "postgres://x@h:5432/fleet", publicStore, ChatSchema},
		{"same database, legacy deployment", legacy, "postgres://x@h:5432/chat", empty, ""},
		{"own schema is verbatim", one, other + "?options=-csearch_path%3Dpublic", empty, PublicSchema},
		{"other database holding a one-database install", one, other, empty, ChatSchema},
		{"other database holding a legacy store", one, other, publicStore, ""},
		{"DR host from a legacy (or unconfigured) box, one-database data", legacy, other, empty, ChatSchema},
		{"DR host from a legacy box, legacy data", legacy, other, publicStore, ""},
		{"probe fails: the deployment decides (one database)", one, other, unreachable, ChatSchema},
		{"probe fails: the deployment decides (legacy)", legacy, other, unreachable, ""},
		{"no probe: the deployment decides", one, other, nil, ChatSchema},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.d.OverrideFor(tc.override, ChatSchema, tc.probe)
			if s := DSNSchema(got); s != tc.want {
				t.Errorf("OverrideFor(%q) = %q (schema %q), want schema %q", tc.override, got, s, tc.want)
			}
			if tc.override != "" && !sameDatabase(got, tc.override) {
				t.Errorf("OverrideFor(%q) = %q points somewhere else", tc.override, got)
			}
		})
	}
	if got := one.ForSched(other); DSNSchema(got) != SchedSchema {
		t.Errorf("ForSched(other) = %q, want the sched schema", got)
	}
}

// TestDSNIdentityNormalises: spellings of one database compare equal (a
// default port, every local host form, URL vs key=value, the user as the
// default database), so neither the layout decision nor the distinct-stores
// guard can be fooled into treating one database as two (#2055 review).
func TestDSNIdentityNormalises(t *testing.T) {
	same := [][]string{
		{"postgres://u@h/db", "postgres://v:pw@H:5432/db?sslmode=require", "host=h dbname=db", "host=h port=5432 dbname=db user=x"},
		{"postgres://u@localhost/db", "postgres://u@127.0.0.1:5432/db", "postgres://u@[::1]/db", "postgres://u@/db", "postgres://u@/db?host=/var/run/postgresql", "host=/tmp dbname=db", "hostaddr=127.0.0.1 dbname=db"},
		{"postgres://fleet@h/fleet", "postgres://fleet@h", "host=h user=fleet"},
	}
	for _, group := range same {
		want := DSNIdentity(group[0])
		if want == "" {
			t.Fatalf("DSNIdentity(%q) is empty", group[0])
		}
		for _, d := range group[1:] {
			if got := DSNIdentity(d); got != want {
				t.Errorf("DSNIdentity(%q) = %q, want %q (same database as %q)", d, got, want, group[0])
			}
		}
	}
	for _, pair := range [][2]string{
		{"postgres://u@h/db", "postgres://u@h:5433/db"},
		{"postgres://u@h/db", "postgres://u@other/db"},
		{"postgres://u@localhost/a", "postgres://u@localhost/b"},
	} {
		if DSNIdentity(pair[0]) == DSNIdentity(pair[1]) {
			t.Errorf("%q and %q compare as one database", pair[0], pair[1])
		}
	}
	// The guard and the layout both see through the spelling.
	if err := EnsureDistinctStores("postgres://u@h/x", "postgres://u@h:5432/x"); err == nil {
		t.Error("two spellings of one database, both in public, were not refused")
	}
	d := ResolveDSNs(func(k string) string {
		return map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://a@localhost/one", "FLEET_SCHED_DATABASE_URL": "postgres://b@127.0.0.1:5432/one"}[k]
	})
	if d.Layout != LayoutSchemas {
		t.Errorf("two spellings of one local database resolved to layout %q", d.Layout)
	}
}

func TestEnsureDistinctStores(t *testing.T) {
	cases := []struct {
		name        string
		chat, sched string
		wantErr     bool
	}{
		{"two databases", "postgres://u@h:5432/chat", "postgres://u@h:5432/sched", false},
		{"one database, two schemas", WithSchema("postgres://u@h:5432/x", "chat"), WithSchema("postgres://u@h:5432/x", "sched"), false},
		{"one database, no schema: both in public", "postgres://a@h:5432/x?sslmode=disable", "postgres://b:pw@h:5432/x?sslmode=require", true},
		{"one database, one schema forced on both", WithSchema("postgres://u@h:5432/x", "chat"), WithSchema("postgres://v@h:5432/x", "chat"), true},
		{"identical key=value DSNs", "host=h dbname=x password=s3cret", "host=h dbname=x password=s3cret", true},
		{"different key=value DSNs", "host=h dbname=x", "host=h dbname=y", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := EnsureDistinctStores(tc.chat, tc.sched)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EnsureDistinctStores(%q, %q) = %v, wantErr %v", tc.chat, tc.sched, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "its own schema") {
				t.Errorf("error %q does not explain the refusal", err)
			}
			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error %q leaks the DSN's password", err)
			}
		})
	}
}
