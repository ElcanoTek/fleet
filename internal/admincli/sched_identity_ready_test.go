package admincli

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/google/uuid"
)

// A sched-only verb that needs accounts, run first on a fresh one-database
// fleet (or mid-upgrade before the chat store migrated), refuses up front
// with what to do, instead of failing on a missing core.users relation
// (Codex 4504695148 on #2083). Once the chat store has created the identity
// table, the same verb works: the scheduler's next open merges.
func TestSchedUserVerbsRefuseBeforeTheIdentityTableExists(t *testing.T) {
	dsn := testdb.Empty(t)
	schedURL := config.WithSchema(dsn, config.SchedSchema)

	verbs := [][]string{
		{"list", "--database-url", schedURL},
		{"add", "ops@x.com", "--password", "placeholder-not-a-secret", "--role", "client", "--database-url", schedURL},
		{"set-role", "ops@x.com", "--role", "readonly", "--database-url", schedURL},
	}
	for _, argv := range verbs {
		var code int
		out := captureStderr(t, func() { code = cmdSchedUser(argv) })
		if code != 1 || !strings.Contains(out, "core.users) does not exist yet") || !strings.Contains(out, "fleet serve") {
			t.Fatalf("sched user %s before chat migrated: exit %d, stderr %q", argv[0], code, out)
		}
		if strings.Contains(out, "relation") {
			t.Fatalf("sched user %s leaked the SQL error: %q", argv[0], out)
		}
	}
	// A legacy bundle with Operations Center users is refused the same way.
	path := writeBundle(t, migrationBundle{
		Format: migrationBundleFormat, Version: 1, Source: "sched-first test",
		Sched: &schedSection{Users: []bundleSchedUser{{ID: uuid.New(), Username: "brad", Role: "client"}}},
	})
	var code int
	out := captureStderr(t, func() { code = cmdImport([]string{path, "--sched-database-url", schedURL}) })
	if code != 1 || !strings.Contains(out, "core.users) does not exist yet") {
		t.Fatalf("import sched users before chat migrated: exit %d, stderr %q", code, out)
	}
	// A verb that needs no accounts still runs sched-first.
	if code := cmdSchedBudget([]string{"list", "--database-url", schedURL}); code != 0 {
		t.Fatalf("sched budget list sched-first: exit %d", code)
	}

	// The chat store creates core.users; now the account verbs work.
	chat, err := store.Open(config.WithSchema(dsn, config.ChatSchema), store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("migrate chat store: %v", err)
	}
	_ = chat.Close()
	if code := cmdSchedUser([]string{"add", "ops@x.com", "--password", "placeholder-not-a-secret", "--role", "client", "--database-url", schedURL}); code != 0 {
		t.Fatalf("sched user add after chat migrated: exit %d", code)
	}
	if code := cmdSchedUser([]string{"list", "--database-url", schedURL}); code != 0 {
		t.Fatalf("sched user list after chat migrated: exit %d", code)
	}
}
