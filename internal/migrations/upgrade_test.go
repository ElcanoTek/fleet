package migrations

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// schemaSnapshot renders the definition of everything in schemas chat, core
// and sched — columns, constraints, indexes, functions, triggers, sequences,
// views, enum types — read from the catalogs, one line per object, sorted.
// The ledgers (core.schema_versions and the two legacy schema_migrations)
// are left out: they are the bookkeeping that differs by history.
func schemaSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	const ledgers = `('chat.schema_migrations', 'sched.schema_migrations', 'core.schema_versions')`
	queries := []string{
		`SELECT format('column %s.%s #%s %s %s(%s) null=%s default=%s', c.table_schema, c.table_name, c.ordinal_position,
			c.column_name, c.udt_name, coalesce(c.character_maximum_length::text, ''), c.is_nullable, coalesce(c.column_default, '-'))
		   FROM information_schema.columns c
		  WHERE c.table_schema IN ('chat', 'core', 'sched') AND (c.table_schema || '.' || c.table_name) NOT IN ` + ledgers,
		`SELECT format('constraint %s.%s %s %s', n.nspname, r.relname, con.conname, pg_get_constraintdef(con.oid))
		   FROM pg_constraint con JOIN pg_class r ON r.oid = con.conrelid JOIN pg_namespace n ON n.oid = r.relnamespace
		  WHERE n.nspname IN ('chat', 'core', 'sched') AND (n.nspname || '.' || r.relname) NOT IN ` + ledgers,
		`SELECT format('index %s', indexdef) FROM pg_indexes
		  WHERE schemaname IN ('chat', 'core', 'sched') AND (schemaname || '.' || tablename) NOT IN ` + ledgers,
		`SELECT format('function %s.%s(%s) %s', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid), md5(pg_get_functiondef(p.oid)))
		   FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE n.nspname IN ('chat', 'core', 'sched') AND p.prokind IN ('f', 'p')`,
		`SELECT format('trigger %s', pg_get_triggerdef(tg.oid)) FROM pg_trigger tg
		   JOIN pg_class r ON r.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = r.relnamespace
		  WHERE NOT tg.tgisinternal AND n.nspname IN ('chat', 'core', 'sched')`,
		`SELECT format('sequence %s.%s %s', schemaname, sequencename, data_type) FROM pg_sequences
		  WHERE schemaname IN ('chat', 'core', 'sched')`,
		`SELECT format('view %s.%s %s', schemaname, viewname, md5(definition)) FROM pg_views
		  WHERE schemaname IN ('chat', 'core', 'sched')`,
		`SELECT format('enum %s.%s %s', n.nspname, ty.typname, string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder))
		   FROM pg_type ty JOIN pg_namespace n ON n.oid = ty.typnamespace JOIN pg_enum e ON e.enumtypid = ty.oid
		  WHERE n.nspname IN ('chat', 'core', 'sched') GROUP BY n.nspname, ty.typname`,
	}
	var out []string
	for _, q := range queries {
		rows, err := db.Query(q + ` ORDER BY 1`)
		if err != nil {
			t.Fatalf("snapshot: %v\n%s", err, q)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		_ = rows.Close()
	}
	return out
}

// diffLines reports the lines only in a and only in b.
func diffLines(a, b []string) (onlyA, onlyB []string) {
	inB := map[string]int{}
	for _, s := range b {
		inB[s]++
	}
	for _, s := range a {
		if inB[s] > 0 {
			inB[s]--
			continue
		}
		onlyA = append(onlyA, s)
	}
	inA := map[string]int{}
	for _, s := range a {
		inA[s]++
	}
	for _, s := range b {
		if inA[s] > 0 {
			inA[s]--
			continue
		}
		onlyB = append(onlyB, s)
	}
	return onlyA, onlyB
}

// freshInstall is what a fresh `fleet serve` builds: Up.
func freshInstall(t *testing.T) *sql.DB {
	t.Helper()
	db := emptyDB(t)
	if err := Up(context.Background(), db); err != nil {
		t.Fatalf("fresh Up: %v", err)
	}
	return db
}

// TestUpgradeFromEarlierVersion verifies upgrading a database from an earlier
// migration version in core.schema_versions holding accounts, tasks, chat
// schedules and a conversation: after Up its schema — chat, core and sched,
// minus the ledgers — is exactly a fresh install's, and every seeded row is
// intact.
func TestUpgradeFromEarlierVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	want := schemaSnapshot(t, freshInstall(t))
	if len(want) < 500 {
		t.Fatalf("the fresh install's snapshot has only %d lines", len(want))
	}

	t.Run("from version 176", func(t *testing.T) {
		t.Parallel()
		db := emptyDB(t)
		if err := UpTo(ctx, db, 176); err != nil {
			t.Fatalf("UpTo 176: %v", err)
		}
		for _, q := range []string{
			`INSERT INTO core.users (id, email, password_hash, role, created_at, updated_at) VALUES ('0b5e4a5e-0000-4c1e-9d1e-0123456789ab', 'alice@example.com', 'h1', 'admin', 1700000000, 1700000000)`,
			`INSERT INTO core.users (id, email, password_hash, role, ops_scope, created_at, updated_at) VALUES ('0b5e4a5e-1111-4c1e-9d1e-0123456789ab', 'ops@example.com', 'h2', 'viewer', 'run', 1700000000, 1700000000)`,
			`INSERT INTO chat.conversations (id, user_email, title, created_at, updated_at) VALUES ('conv-1', 'alice@example.com', 'Deals', 1700000000, 1700000000)`,
			`INSERT INTO sched.tasks (id, prompt, priority, status, created_at) VALUES ('0b5e4a5e-2222-4c1e-9d1e-0123456789ab', 'Check pacing.', 10, 'pending', now())`,
			`INSERT INTO chat.chat_schedules (id, conversation_id, user_email, title, prompt, cron_expr, timezone, duration_seconds, status, next_run_at, run_count, max_runs, created_at, updated_at) VALUES ('sched-1', 'conv-1', 'alice@example.com', '', 'Sweep.', '0 * * * *', 'UTC', 0, 'active', 1700003600, 0, 0, 1700000000, 1700000000)`,
		} {
			mustExec(t, db, q)
		}

		if err := Up(ctx, db); err != nil {
			t.Fatalf("Up: %v", err)
		}
		if a, b := diffLines(want, schemaSnapshot(t, db)); len(a)+len(b) > 0 {
			t.Fatalf("schema after the upgrade differs from a fresh install:\nonly fresh:\n  %s\nonly upgraded:\n  %s", strings.Join(a, "\n  "), strings.Join(b, "\n  "))
		}
		rep, err := Status(ctx, db)
		if err != nil || !rep.Adopted || len(rep.Pending) != 0 {
			t.Fatalf("status after the upgrade: %+v, %v", rep, err)
		}
		var users, tasks, schedules, convs int
		if err := db.QueryRow(`SELECT
			(SELECT count(*) FROM core.users WHERE email IN ('alice@example.com', 'ops@example.com')),
			(SELECT count(*) FROM sched.tasks WHERE prompt = 'Check pacing.'),
			(SELECT count(*) FROM sched.chat_schedules WHERE id = 'sched-1' AND conversation_id = 'conv-1'),
			(SELECT count(*) FROM chat.conversations WHERE id = 'conv-1')`).Scan(&users, &tasks, &schedules, &convs); err != nil {
			t.Fatal(err)
		}
		if users != 2 || tasks != 1 || schedules != 1 || convs != 1 {
			t.Fatalf("seeded rows after the upgrade: users %d tasks %d schedules %d conversations %d", users, tasks, schedules, convs)
		}
		var opsID string
		if err := db.QueryRow(`SELECT id::text FROM core.users WHERE email = 'ops@example.com'`).Scan(&opsID); err != nil || opsID != "0b5e4a5e-1111-4c1e-9d1e-0123456789ab" {
			t.Fatalf("the Operations Center account's id = %q, %v", opsID, err)
		}
	})

	t.Run("from version 170", func(t *testing.T) {
		t.Parallel()
		db := emptyDB(t)
		if err := UpTo(ctx, db, 170); err != nil {
			t.Fatalf("UpTo 170: %v", err)
		}
		if err := Up(ctx, db); err != nil {
			t.Fatalf("Up: %v", err)
		}
		if a, b := diffLines(want, schemaSnapshot(t, db)); len(a)+len(b) > 0 {
			t.Fatalf("schema after the upgrade differs from a fresh install:\nonly fresh:\n  %s\nonly upgraded:\n  %s", strings.Join(a, "\n  "), strings.Join(b, "\n  "))
		}
	})
}
