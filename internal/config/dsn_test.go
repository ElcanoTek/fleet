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

func TestForStoreOverride(t *testing.T) {
	d := ResolveDSNs(func(k string) string {
		return map[string]string{"DATABASE_URL": "postgres://u:pw@h:5432/fleet"}[k]
	})
	if got := d.ForChat(""); got != d.Chat {
		t.Errorf("ForChat(\"\") = %q, want the resolved %q", got, d.Chat)
	}
	// A flag naming the same database inherits the store's schema.
	if got := d.ForSched("postgres://other@h:5432/fleet"); DSNSchema(got) != SchedSchema {
		t.Errorf("ForSched(same db) = %q, want the sched schema", got)
	}
	// A flag naming another database, or carrying its own schema, is verbatim.
	for _, o := range []string{"postgres://u@h:5432/elsewhere", "postgres://u@h:5432/fleet?search_path=x"} {
		if got := d.ForChat(o); got != o {
			t.Errorf("ForChat(%q) = %q, want it verbatim", o, got)
		}
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
