package admincli

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// preMergeDatabase is a box between its first start on the one identity
// table's release and the scheduler's identity merge: every migration applied
// (#2049 M6: one runner migrates chat, core and sched together), the
// scheduler's legacy accounts still in sched.users, the merge not run. Each
// email is both a chat account and a legacy Operations Center account.
func preMergeDatabase(t *testing.T, emails ...string) string {
	t.Helper()
	dsn := testdb.Empty(t)
	if err := migrations.UpDSN(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	chat, err := store.Open(config.WithSchema(dsn, config.ChatSchema), store.DefaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range emails {
		if _, err := raw.Exec(`INSERT INTO sched.users (id, username, password_hash, role, created_at, enabled) VALUES ($1, $2, 'sched-hash-placeholder', 'client', now(), true)`, uuid.New(), e); err != nil {
			t.Fatal(err)
		}
		if _, err := chat.CreateUser(context.Background(), e, "placeholder-not-a-secret"); err != nil {
			t.Fatal(err)
		}
	}
	_ = chat.Close()
	var pending bool
	if err := raw.QueryRow(`SELECT to_regclass('sched.users') IS NOT NULL AND to_regclass('core.users') IS NOT NULL`).Scan(&pending); err != nil || !pending {
		t.Fatalf("not a pre-merge layout (%v)", err)
	}
	return dsn
}

// The first operator verb after an upgrade completes the identity merge
// before it touches an account, so a removed account stays removed: the
// scheduler's next start finds nothing to fold back in (Codex 4505570159 on
// #2083).
func TestAccountVerbsCompleteThePendingMerge(t *testing.T) {
	dsn := preMergeDatabase(t, "gone@x.test", "removed@x.test")
	chatURL := config.WithSchema(dsn, config.ChatSchema)

	if code := cmdChat([]string{"user", "del", "gone@x.test", "--database-url", chatURL}); code != 0 {
		t.Fatalf("chat user del: exit %d", code)
	}
	if code := cmdAdmin([]string{"add", "removed@x.test", "--password", "placeholder-not-a-secret", "--chat-database-url", chatURL}); code != 0 {
		t.Fatalf("admin add: exit %d", code)
	}
	if code := cmdAdmin([]string{"rm", "removed@x.test", "--chat-database-url", chatURL}); code != 0 {
		t.Fatalf("admin rm: exit %d", code)
	}

	// The first scheduler start (fleet serve).
	st := storage.New()
	if err := st.Initialize(config.WithSchema(dsn, config.SchedSchema), fleetdb.SchedPool()); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	defer st.Close()
	if m := st.DB().IdentityMerge(); !m.Ready || m.Ran {
		t.Fatalf("the merge should have completed in the first verb, not now: %+v", m)
	}
	if u, err := st.GetIdentity(t.Context(), "gone@x.test"); err == nil && u != nil {
		t.Fatalf("a deleted account came back after the scheduler started: %+v", u)
	}
	if u, err := st.GetIdentity(t.Context(), "removed@x.test"); err == nil && u != nil {
		t.Fatalf("admin rm was undone: %+v", u)
	}
}
