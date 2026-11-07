// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package config

import "testing"

// TestResolveDSNsLegacy pins the legacy two-database configuration: an
// unconsolidated box must resolve exactly as it did before #2049.
func TestResolveDSNsLegacy(t *testing.T) {
	cases := []struct {
		name                string
		env                 map[string]string
		chat, sched         string
		chatFrom, schedFrom string
		layout              Layout
	}{
		{
			name:     "nothing set: localhost defaults per store",
			env:      nil,
			chat:     "postgres://chat@localhost:5432/chat?sslmode=disable",
			sched:    "postgres://fleet@localhost:5432/sched?sslmode=disable",
			chatFrom: DSNFromDefaults, schedFrom: DSNFromDefaults,
			layout: LayoutLegacy,
		},
		{
			name:     "store-specific URLs win over everything",
			env:      map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://c/chat", "FLEET_SCHED_DATABASE_URL": "postgres://s/sched", "SCHED_DATABASE_URL": "postgres://legacy/sched", "DATABASE_URL": "postgres://g/db", "DB_NAME": "x"},
			chat:     "postgres://c/chat",
			sched:    "postgres://s/sched",
			chatFrom: "FLEET_CHAT_DATABASE_URL", schedFrom: "FLEET_SCHED_DATABASE_URL",
			layout: LayoutLegacy,
		},
		{
			name:     "SCHED_DATABASE_URL beats DATABASE_URL for sched only",
			env:      map[string]string{"SCHED_DATABASE_URL": "postgres://legacy/sched", "DATABASE_URL": "postgres://g/db"},
			chat:     "postgres://g/db",
			sched:    "postgres://legacy/sched",
			chatFrom: "DATABASE_URL", schedFrom: "SCHED_DATABASE_URL",
			layout: LayoutLegacy,
		},
		{
			name:     "DB_* parts without DB_NAME keep the per-store defaults",
			env:      map[string]string{"DB_HOST": "db.internal", "DB_PORT": "6543", "DB_PASSWORD": "p@ss #1", "DB_SSLMODE": "require"},
			chat:     "postgres://chat:p%40ss%20%231@db.internal:6543/chat?sslmode=require",
			sched:    "postgres://fleet:p%40ss%20%231@db.internal:6543/sched?sslmode=require",
			chatFrom: DSNFromDBParts, schedFrom: DSNFromDBParts,
			layout: LayoutLegacy,
		},
		{
			name:     "both store keys naming one database is the one-database layout",
			env:      map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://a@h:5432/one", "FLEET_SCHED_DATABASE_URL": "postgres://b@h:5432/one"},
			chat:     "postgres://a@h:5432/one?" + chatOpt,
			sched:    "postgres://b@h:5432/one?" + schedOpt,
			chatFrom: "FLEET_CHAT_DATABASE_URL", schedFrom: "FLEET_SCHED_DATABASE_URL",
			layout: LayoutSchemas,
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
			if d.Layout != tc.layout || LayoutOf(d.Chat, d.Sched) != tc.layout {
				t.Errorf("layout = %q / LayoutOf %q, want %q", d.Layout, LayoutOf(d.Chat, d.Sched), tc.layout)
			}
			if err := EnsureDistinctStores(d.Chat, d.Sched); err != nil {
				t.Errorf("EnsureDistinctStores refused the resolved pair: %v", err)
			}
		})
	}
}
