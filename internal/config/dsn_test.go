package config

import (
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
			name:  "nothing configured: the bare defaults are ONE database, fleet/fleet",
			env:   map[string]string{},
			chat:  "postgres://fleet@localhost:5432/fleet?sslmode=disable&" + chatOpt,
			sched: "postgres://fleet@localhost:5432/fleet?sslmode=disable&" + schedOpt,
			from:  DSNFromDefaults,
		},
		{
			name:  "DB_USER alone keeps the fleet database",
			env:   map[string]string{"DB_USER": "app"},
			chat:  "postgres://app@localhost:5432/fleet?sslmode=disable&" + chatOpt,
			sched: "postgres://app@localhost:5432/fleet?sslmode=disable&" + schedOpt,
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
			d, err := ResolveDSNs(func(k string) string { return tc.env[k] })
			if err != nil {
				t.Fatalf("ResolveDSNs: %v", err)
			}
			if d.Chat != tc.chat || d.Sched != tc.sched {
				t.Errorf("DSNs = (%q, %q), want (%q, %q)", d.Chat, d.Sched, tc.chat, tc.sched)
			}
			if d.From != tc.from {
				t.Errorf("From = %q, want %q", d.From, tc.from)
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

// TestResolveDSNsRefusesRetiredKeys: the per-store keys of the removed
// two-database layout fail loudly, naming DATABASE_URL and never a value —
// alone, next to DATABASE_URL, or naming one database (#2049 phase 2).
func TestResolveDSNsRefusesRetiredKeys(t *testing.T) {
	for _, env := range []map[string]string{
		{"FLEET_CHAT_DATABASE_URL": "postgres://c:s3cret@h:5432/chat"},
		{"FLEET_SCHED_DATABASE_URL": "postgres://s:s3cret@h:5432/sched", "DATABASE_URL": "postgres://u@h/fleet"},
		{"SCHED_DATABASE_URL": "postgres://s:s3cret@h:5432/sched"},
		{"FLEET_CHAT_DATABASE_URL": "postgres://a:s3cret@h/one", "FLEET_SCHED_DATABASE_URL": "postgres://a:s3cret@h/one"},
	} {
		_, err := ResolveDSNs(func(k string) string { return env[k] })
		if err == nil {
			t.Errorf("ResolveDSNs(%v) accepted a retired per-store key", env)
			continue
		}
		for k := range env {
			if k != "DATABASE_URL" && !strings.Contains(err.Error(), k) {
				t.Errorf("error %q does not name %s", err, k)
			}
		}
		if !strings.Contains(err.Error(), "set DATABASE_URL") || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error %q must name DATABASE_URL and no value", err)
		}
	}
	// Set but empty (KEY="" in an env file, whitespace) is unset, as everywhere.
	if _, err := ResolveDSNs(func(k string) string {
		return map[string]string{"FLEET_CHAT_DATABASE_URL": "  ", "DATABASE_URL": "postgres://u@h/fleet"}[k]
	}); err != nil {
		t.Errorf("an empty retired key was refused: %v", err)
	}
}

// TestOverrideFor pins how a --database-url override is resolved: verbatim
// when it names its own schema, else in the store's schema — whatever
// database it names (#2055 review: an override naming another database used
// to stay in public).
func TestOverrideFor(t *testing.T) {
	one, err := ResolveDSNs(func(k string) string {
		return map[string]string{"DATABASE_URL": "postgres://u:pw@h:5432/fleet"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	const other = "postgres://u@dr-host:5432/fleet"
	cases := []struct {
		name     string
		override string
		want     string // the schema the result names
	}{
		{"no override is the resolved DSN", "", ChatSchema},
		{"same database", "postgres://x@h:5432/fleet", ChatSchema},
		{"own schema is verbatim", other + "?options=-csearch_path%3Dpublic", PublicSchema},
		{"another database", other, ChatSchema},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := one.OverrideFor(tc.override, ChatSchema)
			if s := DSNSchema(got); s != tc.want {
				t.Errorf("OverrideFor(%q) = %q (schema %q), want schema %q", tc.override, got, s, tc.want)
			}
			if tc.override != "" && !sameDatabase(got, tc.override) {
				t.Errorf("OverrideFor(%q) = %q points somewhere else", tc.override, got)
			}
		})
	}
	if got := one.OverrideFor(other, SchedSchema); DSNSchema(got) != SchedSchema {
		t.Errorf("OverrideFor(other, sched) = %q, want the sched schema", got)
	}
}

// TestDSNIdentityNormalises: spellings of one database compare equal (a
// default port, every local host form, URL vs key=value, the user as the
// default database), so the distinct-stores guard cannot be fooled into
// treating one database as two (#2055 review).
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
	// The guard sees through the spelling.
	if err := EnsureDistinctStores("postgres://u@h/x", "postgres://u@h:5432/x"); err == nil {
		t.Error("two spellings of one database, both in public, were not refused")
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
