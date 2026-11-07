package config

import (
	"strings"
	"testing"
)

func TestResolveDSNsPrecedence(t *testing.T) {
	cases := []struct {
		name                string
		env                 map[string]string
		chat, sched         string
		chatFrom, schedFrom string
	}{
		{
			name:     "nothing set: localhost defaults per store",
			env:      nil,
			chat:     "postgres://chat@localhost:5432/chat?sslmode=disable",
			sched:    "postgres://fleet@localhost:5432/sched?sslmode=disable",
			chatFrom: DSNFromDefaults, schedFrom: DSNFromDefaults,
		},
		{
			name:     "store-specific URLs win over everything",
			env:      map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://c/chat", "FLEET_SCHED_DATABASE_URL": "postgres://s/sched", "SCHED_DATABASE_URL": "postgres://legacy/sched", "DATABASE_URL": "postgres://g/db", "DB_NAME": "x"},
			chat:     "postgres://c/chat",
			sched:    "postgres://s/sched",
			chatFrom: "FLEET_CHAT_DATABASE_URL", schedFrom: "FLEET_SCHED_DATABASE_URL",
		},
		{
			name:     "SCHED_DATABASE_URL beats DATABASE_URL for sched only",
			env:      map[string]string{"SCHED_DATABASE_URL": "postgres://legacy/sched", "DATABASE_URL": "postgres://g/db"},
			chat:     "postgres://g/db",
			sched:    "postgres://legacy/sched",
			chatFrom: "DATABASE_URL", schedFrom: "SCHED_DATABASE_URL",
		},
		{
			name:     "DATABASE_URL beats DB_* parts",
			env:      map[string]string{"DATABASE_URL": "  postgres://g/db  ", "DB_HOST": "db.internal"},
			chat:     "postgres://g/db",
			sched:    "postgres://g/db",
			chatFrom: "DATABASE_URL", schedFrom: "DATABASE_URL",
		},
		{
			name:     "DB_* parts build URL form with escaping and per-store defaults",
			env:      map[string]string{"DB_HOST": "db.internal", "DB_PORT": "6543", "DB_PASSWORD": "p@ss #1", "DB_SSLMODE": "require"},
			chat:     "postgres://chat:p%40ss%20%231@db.internal:6543/chat?sslmode=require",
			sched:    "postgres://fleet:p%40ss%20%231@db.internal:6543/sched?sslmode=require",
			chatFrom: DSNFromDBParts, schedFrom: DSNFromDBParts,
		},
		{
			name:     "an IPv6 DB_HOST is bracketed in the authority",
			env:      map[string]string{"DB_HOST": "2001:db8::1"},
			chat:     "postgres://chat@[2001:db8::1]:5432/chat?sslmode=disable",
			sched:    "postgres://fleet@[2001:db8::1]:5432/sched?sslmode=disable",
			chatFrom: DSNFromDBParts, schedFrom: DSNFromDBParts,
		},
		{
			name:     "a Unix-socket DB_HOST moves host and port into the query",
			env:      map[string]string{"DB_HOST": "/var/run/postgresql"},
			chat:     "postgres://chat@/chat?host=%2Fvar%2Frun%2Fpostgresql&port=5432&sslmode=disable",
			sched:    "postgres://fleet@/sched?host=%2Fvar%2Frun%2Fpostgresql&port=5432&sslmode=disable",
			chatFrom: DSNFromDBParts, schedFrom: DSNFromDBParts,
		},
		{
			name:     "DB_PASSWORD keeps surrounding whitespace (an opaque secret)",
			env:      map[string]string{"DB_HOST": " db ", "DB_PASSWORD": " pw "},
			chat:     "postgres://chat:%20pw%20@db:5432/chat?sslmode=disable",
			sched:    "postgres://fleet:%20pw%20@db:5432/sched?sslmode=disable",
			chatFrom: DSNFromDBParts, schedFrom: DSNFromDBParts,
		},
		{
			name:     "DB_USER/DB_NAME apply to both stores",
			env:      map[string]string{"DB_USER": "app", "DB_NAME": "one"},
			chat:     "postgres://app@localhost:5432/one?sslmode=disable",
			sched:    "postgres://app@localhost:5432/one?sslmode=disable",
			chatFrom: DSNFromDBParts, schedFrom: DSNFromDBParts,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ResolveDSNs(func(k string) string { return tc.env[k] })
			if d.Chat != tc.chat || d.Sched != tc.sched {
				t.Errorf("DSNs = (%q, %q), want (%q, %q)", d.Chat, d.Sched, tc.chat, tc.sched)
			}
			if d.ChatFrom != tc.chatFrom || d.SchedFrom != tc.schedFrom {
				t.Errorf("From = (%q, %q), want (%q, %q)", d.ChatFrom, d.SchedFrom, tc.chatFrom, tc.schedFrom)
			}
		})
	}
}

func TestEnsureDistinctDatabases(t *testing.T) {
	resolved := func(env map[string]string) DSNs {
		return ResolveDSNs(func(k string) string { return env[k] })
	}
	cases := []struct {
		name    string
		d       DSNs
		wantErr bool
	}{
		{"defaults are distinct", resolved(nil), false},
		{"store-specific URLs distinct", resolved(map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://u@h:5432/chat", "FLEET_SCHED_DATABASE_URL": "postgres://u@h:5432/sched"}), false},
		{"only DATABASE_URL set is refused", resolved(map[string]string{"DATABASE_URL": "postgres://u@h:5432/fleet"}), true},
		// #2018: a DB_*-only config naming one DB_NAME resolves both stores to
		// the same database under different default users; the comparison
		// is by host:port/db, so it must be refused.
		{"shared DB_NAME via DB_* parts is refused", resolved(map[string]string{"DB_HOST": "db", "DB_NAME": "fleet"}), true},
		{"same db, different user and query is refused", DSNs{Chat: "postgres://a@h:5432/x?sslmode=disable", Sched: "postgres://b:pw@h:5432/x?sslmode=require"}, true},
		{"identical key=value DSNs are refused", DSNs{Chat: "host=h dbname=x password=s3cret", Sched: "host=h dbname=x password=s3cret"}, true},
		{"different key=value DSNs pass", DSNs{Chat: "host=h dbname=x", Sched: "host=h dbname=y"}, false},
		{"one socket directory, one database name is refused", resolved(map[string]string{"DB_HOST": "/var/run/postgresql", "DB_NAME": "fleet"}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := EnsureDistinctDatabases(tc.d.Chat, tc.d.Sched)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EnsureDistinctDatabases(%q, %q) = %v, wantErr %v", tc.d.Chat, tc.d.Sched, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "SEPARATE databases") {
				t.Errorf("error %q does not explain the refusal", err)
			}
			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error %q leaks the DSN's password", err)
			}
		})
	}
}
