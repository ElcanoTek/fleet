// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package admincli

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// consolidateServer returns the test server's maintenance DSN
// (internal/testdb) and an execPG that runs the client tools against it as
// the test role (standing in for `runuser -u postgres`), skipping when no
// server or no client tools are available.
func consolidateServer(t *testing.T) (*url.URL, *execPG, *sql.DB) {
	t.Helper()
	base := testdb.MaintenanceDSN(t)
	for _, b := range []string{"psql", "pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	var super bool
	if err := admin.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil || !super {
		t.Skipf("the test role is not a superuser (%v); the e2e consolidation needs one", err)
	}
	pw, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	pg := &execPG{
		connArgs: []string{"-h", u.Hostname(), "-p", port, "-U", u.User.Username()},
		env:      []string{"PGPASSWORD=" + pw, "PGSSLMODE=" + orStr(u.Query().Get("sslmode"), "prefer")},
	}
	return u, pg, admin
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func dbURL(base *url.URL, name string) string {
	u := *base
	u.Path = "/" + name
	return u.String()
}

// seedLegacyPair fills a legacy pair through the real stores: three chat
// users, one sched user, and one chat sequence advanced to 4242 so the copy
// must carry a non-trivial value. It returns that sequence's name.
func seedLegacyPair(t *testing.T, chatDSN, schedDSN string) string {
	t.Helper()
	ctx := t.Context()
	cs, err := store.Open(chatDSN, store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open legacy chat: %v", err)
	}
	for _, e := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if _, err := cs.CreateUser(ctx, e, "placeholder-password-1"); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}
	_ = cs.Close()
	src, err := sql.Open("pgx", chatDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var seqName string
	if err := src.QueryRow(`SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' ORDER BY 1 LIMIT 1`).Scan(&seqName); err != nil {
		t.Fatalf("find a chat sequence: %v", err)
	}
	if _, err := src.Exec(`SELECT setval(format('public.%I', $1::text), 4242)`, seqName); err != nil {
		t.Fatalf("setval: %v", err)
	}
	sd := &scheddb.Database{}
	if err := sd.Init(schedDSN, scheddb.DefaultPoolConfig()); err != nil {
		t.Fatalf("open legacy sched: %v", err)
	}
	defer sd.Close()
	if err := sd.AddUser(ctx, &models.User{ID: uuid.New(), Username: "a@example.com", PasswordHash: "x", Role: "admin", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	return seqName
}

// checkSourcesIntact: the chat source still holds its rows in public, and no
// scratch copy is left behind.
func checkSourcesIntact(t *testing.T, admin *sql.DB, chatDSN, target string) {
	t.Helper()
	// A successful move leaves the sources closed to connections; re-open
	// the chat source as the printed rollback would, to look inside.
	db := strings.TrimPrefix(strings.SplitN(strings.SplitN(chatDSN, "?", 2)[0], "/", 4)[3], "/")
	var allow bool
	if err := admin.QueryRow(`SELECT datallowconn FROM pg_database WHERE datname = $1`, db).Scan(&allow); err != nil || allow {
		t.Errorf("source %s datallowconn = %v (%v), want closed after the move", db, allow, err)
	}
	if _, err := admin.Exec(`ALTER DATABASE ` + db + ` ALLOW_CONNECTIONS true`); err != nil {
		t.Fatal(err)
	}
	src, err := sql.Open("pgx", chatDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var n int
	if err := src.QueryRow(`SELECT count(*) FROM public.users`).Scan(&n); err != nil || n != 3 {
		t.Errorf("source chat users = %d, %v", n, err)
	}
	if err := admin.QueryRow(`SELECT count(*) FROM pg_database WHERE datname LIKE $1`, target+"_consolidate_%").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d scratch database(s) left behind (%v)", n, err)
	}
}

// TestConsolidateEndToEnd moves a real legacy install — two databases filled
// by the real migrations plus rows — into one database with the real client
// tools, and checks the result through the real stores.
func TestConsolidateEndToEnd(t *testing.T) { runConsolidateE2E(t, nil) }

// TestConsolidateEndToEndRunuser is the same move through the PRODUCTION
// runner: runuser -u postgres, peer authentication, aimed at the DSNs'
// instance (superuserPG). It needs root, runuser and a local cluster the
// postgres OS user can reach on its default socket; skipped otherwise.
func TestConsolidateEndToEndRunuser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (runuser -u postgres)")
	}
	if _, err := exec.LookPath("runuser"); err != nil {
		t.Skip("runuser not installed")
	}
	if out, err := exec.Command("runuser", "-u", "postgres", "--", "psql", "-XAtqc", "SELECT 1").CombinedOutput(); err != nil {
		t.Skipf("runuser -u postgres -- psql does not reach a local cluster: %v: %s", err, out)
	}
	runConsolidateE2E(t, superuserPG)
}

// runConsolidateE2E runs the end-to-end move. With pgFor nil the client tools
// run as the test role (consolidateServer); otherwise pgFor builds the runner
// from the plan, as in production.
func runConsolidateE2E(t *testing.T, pgFor func(*consolidatePlan) pgSuper) {
	// The server comes from the environment first: clearing the DSN keys
	// before reading it skipped the test whenever only DATABASE_URL named it.
	base, pg, admin := consolidateServer(t)
	clearDSNEnv(t)
	ctx := t.Context()
	// The legacy pair and the target are per-test databases (internal/testdb):
	// dropped at cleanup, garbage-collected after a crash. The target does not
	// exist yet — the tool creates it, and its scratch copies are named after
	// it (<target>_consolidate_<store>), inside testdb's prefix too.
	chatDSN, schedDSN := testdb.LegacyPair(t)
	target, _ := testdb.Reserve(t)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, n := range []string{target + "_consolidate_chat", target + "_consolidate_sched"} {
			_, _ = admin.ExecContext(cctx, `DROP DATABASE IF EXISTS `+n+` WITH (FORCE)`)
		}
	})
	seqName := seedLegacyPair(t, chatDSN, schedDSN)

	envFile := filepath.Join(t.TempDir(), "fleet.env")
	body := "OPENROUTER_API_KEY=placeholder\nFLEET_CHAT_DATABASE_URL=" + chatDSN + "\nFLEET_SCHED_DATABASE_URL=" + schedDSN + "\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := func(out *bytes.Buffer) consolidateDeps {
		return consolidateDeps{
			pg:    pg,
			pgFor: pgFor,
			// The seed step's pools have just closed; their backends can
			// linger in pg_stat_activity for a moment (seen under -race).
			connWait:     15 * time.Second,
			unitState:    func(string) string { return "" },
			freeBytes:    freeBytesAt,
			verifyStores: verifyStoresOpen,
			dsnQuery:     queryDSN,
			now:          time.Now,
			out:          out,
		}
	}
	opts := consolidateOptions{envFile: envFile, targetDB: target}

	// Dry run: plan with exact counts, nothing created.
	var out bytes.Buffer
	if code := runConsolidate(ctx, opts, deps(&out)); code != 0 {
		t.Fatalf("dry run exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "chat.users") || !strings.Contains(out.String(), "Dry run complete") {
		t.Fatalf("dry run output:\n%s", out.String())
	}
	var n int
	_ = admin.QueryRow(`SELECT count(*) FROM pg_database WHERE datname = $1`, target).Scan(&n)
	if n != 0 {
		t.Fatal("dry run created the target")
	}

	// A half-filled target from an "earlier failed run" is refused, then
	// cleaned with --force-clean.
	if _, err := admin.Exec(`CREATE DATABASE ` + target); err != nil {
		t.Fatal(err)
	}
	half, err := sql.Open("pgx", dbURL(base, target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := half.Exec(`CREATE SCHEMA chat; CREATE TABLE chat.junk (x int)`); err != nil {
		t.Fatal(err)
	}
	_ = half.Close()
	opts.apply = true
	out.Reset()
	if code := runConsolidate(ctx, opts, deps(&out)); code != 1 || !strings.Contains(out.String(), "--force-clean") {
		t.Fatalf("half-filled target: exit %d:\n%s", code, out.String())
	}
	opts.forceClean = true
	out.Reset()
	if code := runConsolidate(ctx, opts, deps(&out)); code != 0 {
		t.Fatalf("apply exit %d:\n%s", code, out.String())
	}
	t.Logf("apply output:\n%s", out.String())
	if !strings.Contains(out.String(), "identical") {
		t.Errorf("apply output lacks the verification summary:\n%s", out.String())
	}

	// The env file now names the one database.
	vals, err := creds.ReadEnvValues(envFile)
	if err != nil {
		t.Fatal(err)
	}
	d := config.ResolveDSNs(func(k string) string { return vals[k] })
	if d.Layout != config.LayoutSchemas || config.DSNIdentity(d.Chat) != config.DSNIdentity(dbURL(base, target)) {
		t.Fatalf("rewritten env resolves to %+v", d)
	}

	// Both stores open in schema mode with their data, migrations no-ops.
	cs, err := store.Open(d.Chat, store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open consolidated chat: %v", err)
	}
	users, err := cs.ListUsers(ctx)
	_ = cs.Close()
	if err != nil || len(users) != 3 {
		t.Fatalf("consolidated chat users = %d, %v; want 3", len(users), err)
	}
	sd := &scheddb.Database{}
	if err := sd.Init(d.Sched, scheddb.DefaultPoolConfig()); err != nil {
		t.Fatalf("open consolidated sched: %v", err)
	}
	if c, err := sd.CountUsers(ctx); err != nil || c != 1 {
		t.Errorf("consolidated sched users = %d, %v; want 1", c, err)
	}
	_ = sd.Close()

	// Sequences kept their values.
	tdb, err := sql.Open("pgx", dbURL(base, target))
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close()
	var last int64
	if err := tdb.QueryRow(`SELECT last_value FROM pg_sequences WHERE schemaname = 'chat' AND sequencename = $1`, seqName).Scan(&last); err != nil || last != 4242 {
		t.Errorf("chat.%s last_value = %d, %v; want 4242", seqName, last, err)
	}
	var owner string
	if err := tdb.QueryRow(`SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'sched'`).Scan(&owner); err != nil || owner != base.User.Username() {
		t.Errorf("sched schema owner = %q, %v", owner, err)
	}

	checkSourcesIntact(t, admin, chatDSN, target)

	// A re-run is a no-op.
	out.Reset()
	if code := runConsolidate(ctx, opts, deps(&out)); code != 0 || !strings.Contains(out.String(), "Nothing to do") {
		t.Errorf("re-run exit %d:\n%s", code, out.String())
	}
}
