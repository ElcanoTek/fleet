package dbschema_test

import (
	"database/sql"
	"net/url"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestStoresOpenWithoutUsageOnPublic: in a hardened database where the
// stores' role has no USAGE on public, the legacy-store probe must still run
// (it reads the catalogs; to_regclass('public.…') would fail outright and
// keep every store from opening).
func TestStoresOpenWithoutUsageOnPublic(t *testing.T) {
	t.Parallel()

	const role, pw = "fleet_hardened_test", "placeholder-not-a-secret"
	// The role is server-wide, so its name is fixed. It can only be dropped
	// once the database holding its grants and schemas is gone, and testdb
	// may drop that in the background after the test — so the test drops the
	// scratch database itself, then the role, through the maintenance
	// connection.
	maint, err := sql.Open("pgx", testdb.MaintenanceDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = maint.Close() })
	dsn := scratchDatabase(t)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	_, _ = maint.Exec(`DROP ROLE IF EXISTS ` + role)
	if _, err := maint.Exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + pw + `'`); err != nil {
		t.Fatalf("create a role: %v", err)
	}
	u, _ := url.Parse(dsn)
	dbName := u.Path[1:]
	t.Cleanup(func() {
		_, _ = maint.Exec(`DROP DATABASE IF EXISTS "` + dbName + `" WITH (FORCE)`)
		_, _ = maint.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	if _, err := admin.Exec(`REVOKE ALL ON SCHEMA public FROM PUBLIC; GRANT CREATE ON DATABASE ` + dbName + ` TO ` + role); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, pw)
	roleDSN := u.String()

	chat, err := store.Open(config.WithSchema(roleDSN, config.ChatSchema), store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("chat store without USAGE on public: %v", err)
	}
	_ = chat.Close()
	sched := &scheddb.Database{}
	err = sched.Init(config.WithSchema(roleDSN, config.SchedSchema), scheddb.DefaultPoolConfig())
	_ = sched.Close()
	if err != nil {
		t.Fatalf("sched store without USAGE on public: %v", err)
	}
}
