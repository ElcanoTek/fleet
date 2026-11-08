// Package testdb gives every Postgres-backed test a database of its own
// (#2019, #2049 M2).
//
// A test asks for a fresh database and gets one, cloned from a migrated
// template with CREATE DATABASE … TEMPLATE and dropped when the test ends. No
// two tests ever share a database, so there is nothing to wipe between tests,
// no advisory lock to serialize on, and the DB packages run in parallel with
// each other and — where a test allows it (t.Parallel) — within themselves.
//
// The one knob is FLEET_TEST_DATABASE_URL: a DSN (URL form) for ANY database
// on the test server, used only as the maintenance connection testdb creates
// and drops databases through. testdb never writes to that database. The role
// needs CREATEDB (or superuser). The rule on skipping is unchanged from #2019:
// a test skips only when FLEET_TEST_DATABASE_URL is EMPTY (a laptop without
// Postgres); once it is set, every connect, privilege, clone or migration
// error is a test failure.
//
// What a test gets:
//
//   - New(t, spec): a clone of the template spec describes — a database
//     already migrated by spec.Migrate. Templates are built once per migration
//     set and reused across processes, runs and worktrees: the template's
//     name carries a hash of spec.Fingerprint (the embedded migration files),
//     so two worktrees on different migration sets never share one.
//   - Empty(t): a fresh empty database (migration-runner tests and others
//     that fill it themselves).
//
// Every DB package clones one template, internal/testdb/onedb.Spec: the whole
// database migrated by the one runner (internal/migrations — schemas chat,
// core and sched, the production layout, ADR-0098 and ADR-0107). The stores'
// own internal tests use it through New directly (internal/store,
// internal/sched/db, which cannot import a package that imports them); every
// other package uses internal/testdb/fleetdb, which opens the stores on it.
//
// # Names and garbage collection
//
// testdb creates and drops only databases whose names start with fleet_tmpl_
// (templates; fleet_tmpl_build_ while one is being migrated) or fleet_t_
// (per-test databases), and never anything else on the server.
//
// Each test process holds a session advisory lock, on its maintenance
// connection, keyed by a random owner id that is also part of every per-test
// database name (fleet_t_<owner>_<n>) and template build name. When a
// process dies without running its cleanups (a killed or timed-out run), its
// lock goes with its connection. The next process to start garbage-collects:
// under the build lock it drops every fleet_t_ and fleet_tmpl_build_ database
// whose owner holds no owner lock in pg_locks (which spans every database of
// the server, so a run with another maintenance database still counts as
// alive). A database whose owner is alive is never touched. When it builds a
// template it also drops all but the newest three templates of that kind.
// `scripts/test-db-setup.sh --gc` does the same by hand.
//
// Point every run on one server at the same maintenance database: the build
// lock is an advisory lock, which Postgres scopes to one database. (Two runs
// that do not still work — a build that loses the race uses the winner's
// template — they may just migrate the same template twice.)
package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // the pgx database/sql driver
)

// EnvVar names the maintenance DSN (see the package comment).
const EnvVar = "FLEET_TEST_DATABASE_URL"

// StrategyEnvVar optionally forces CREATE DATABASE's STRATEGY for clones:
// "wal_log" or "file_copy". Unset uses the server's default (WAL_LOG on
// Postgres 15+), which measured faster for fleet's small templates.
const StrategyEnvVar = "FLEET_TEST_DB_STRATEGY"

// PoolMaxOpen and PoolMaxIdle size the connection pools the test helpers open
// on a per-test database. Dozens of tests run at once (packages in parallel,
// t.Parallel within them), so each keeps a small pool to stay well under the
// server's max_connections. A test that exercises pool contention asks for
// more explicitly.
const (
	PoolMaxOpen = 4
	PoolMaxIdle = 2
)

// Database name prefixes. Everything testdb creates or drops matches one of
// them; garbage collection is confined to them.
const (
	templatePrefix = "fleet_tmpl_"
	buildPrefix    = "fleet_tmpl_build_"
	clonePrefix    = "fleet_t_"
)

// recipeVersion is folded into every template's key. Bump it when the way a
// template is built changes in a way its migration fingerprint does not show.
const recipeVersion = "testdb-1"

// keepTemplates is how many templates of one kind (Spec.Name) survive
// garbage collection: the current one plus a couple a concurrent run on an
// older or newer migration set (another worktree) may still be cloning.
const keepTemplates = 3

// Advisory-lock namespaces (the two-int4 key form; the first int is the
// namespace), held in the maintenance database. Advisory locks are per
// database, which is why every process locks through the maintenance DSN.
const (
	lockNSOwner int32 = 0x666c7474 // "fltt": one per live test process
	lockNSBuild int32 = 0x666c7462 // "fltb": template builds and GC
)

// Spec describes a template: what to migrate an empty database with, and the
// fingerprint that decides when the template must be rebuilt.
type Spec struct {
	// Name is the template's kind: lower-case letters, at most 12.
	Name string
	// Fingerprint returns a value that changes whenever Migrate would build a
	// different database — normally a hash of the embedded migration files.
	Fingerprint func() string
	// Migrate migrates the empty database dsn names and closes every
	// connection it opened before returning.
	Migrate func(ctx context.Context, dsn string) error
}

var specName = regexp.MustCompile(`^[a-z]{1,12}$`)

// cloneName parses a per-test database name: fleet_t_<owner hex>_<n>, plus
// an optional suffix for a database a tool under test derives from a
// per-test name (garbage-collected with it).
var cloneName = regexp.MustCompile(`^fleet_t_([0-9a-f]{8})_[0-9]+(_[a-z0-9_]+)?$`)

// buildName parses a template build in progress:
// fleet_tmpl_build_<spec name>_<key hash>_<owner hex>.
var buildName = regexp.MustCompile(`^fleet_tmpl_build_[a-z]+_[0-9a-f]{12}_([0-9a-f]{8})$`)

// server is the per-process maintenance state, created on first use.
type server struct {
	base     *url.URL
	admin    *sql.DB
	lockConn *sql.Conn // holds this process's owner lock for its lifetime
	owner    uint32
	seq      atomic.Uint64
	strategy string
	role     string // the test role (current_user); part of every template key
	started  bool   // the first collect has run

	mu    sync.Mutex      // one template build at a time in this process
	ready map[string]bool // templates known to exist

	bg background // Run's background mode (run.go)
}

var (
	srvOnce sync.Once
	srv     *server
	errSrv  error
)

// get returns the process's server, skipping the test when EnvVar is empty and
// failing it on any other problem.
func get(t testing.TB) *server {
	t.Helper()
	dsn := strings.TrimSpace(envreg.Get(EnvVar))
	if dsn == "" {
		t.Skip(EnvVar + " is not set — skipping Postgres-backed test")
	}
	srvOnce.Do(func() { srv, errSrv = connect(dsn) })
	if errSrv != nil {
		t.Fatalf("testdb: %v", errSrv)
	}
	return srv
}

func connect(dsn string) (*server, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, fmt.Errorf("%s must be a postgres:// URL naming any database on the test server", EnvVar)
	}
	strategy := strings.ToLower(strings.TrimSpace(envreg.Get(StrategyEnvVar)))
	switch strategy {
	case "", "wal_log", "file_copy":
	default:
		return nil, fmt.Errorf("%s=%q: want wal_log or file_copy", StrategyEnvVar, strategy)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", EnvVar, err)
	}
	// One connection is pinned for the owner lock; the rest serve CREATE and
	// DROP DATABASE, which the server serializes heavily anyway.
	admin.SetMaxOpenConns(8)
	admin.SetMaxIdleConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := &server{base: u, admin: admin, strategy: strategy, ready: map[string]bool{}}
	if err := s.init(ctx); err != nil {
		_ = admin.Close()
		return nil, err
	}
	s.startBackground()
	live.Store(s)
	return s, nil
}

func (s *server) init(ctx context.Context) error {
	if err := s.admin.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to %s: %w", EnvVar, err)
	}
	var role string
	var can bool
	if err := s.admin.QueryRowContext(ctx,
		`SELECT current_user, rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&role, &can); err != nil {
		return fmt.Errorf("read the test role's privileges: %w", err)
	}
	if !can {
		return fmt.Errorf("role %q cannot create databases; every DB test gets its own database. Run: ALTER ROLE %s CREATEDB", role, quoteIdent(role))
	}
	conn, err := s.admin.Conn(ctx)
	if err != nil {
		return fmt.Errorf("maintenance connection: %w", err)
	}
	s.role = role
	s.lockConn = conn
	// A random owner id, unique among live processes: the lock is taken with
	// try, so a collision with a live process just draws again.
	for range 64 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		owner := binary.BigEndian.Uint32(b[:]) & math.MaxInt32
		key, _ := ownerLockKey(uint64(owner))
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1::int4, $2::int4)`, lockNSOwner, key).Scan(&got); err != nil {
			return fmt.Errorf("owner lock: %w", err)
		}
		if got {
			s.owner = owner
			if err := s.withBuildLock(ctx, s.collect); err != nil {
				return err
			}
			s.started = true
			return nil
		}
	}
	return errors.New("could not take a free owner lock")
}

// ownerLockKey is the int4 half of an owner's advisory-lock key. Owner ids
// are drawn as 31-bit values, so the key is the id itself; anything larger is
// not an owner this package created.
func ownerLockKey(id uint64) (int32, bool) {
	if id > math.MaxInt32 {
		return 0, false
	}
	return int32(id), true
}

func (s *server) ownerHex() string { return fmt.Sprintf("%08x", s.owner) }

// withBuildLock runs fn holding the build lock — the one lock every template
// build and every garbage collection takes.
func (s *server) withBuildLock(ctx context.Context, fn func(context.Context) error) error {
	if _, err := s.lockConn.ExecContext(ctx, `SELECT pg_advisory_lock($1::int4, 0)`, lockNSBuild); err != nil {
		return fmt.Errorf("build lock: %w", err)
	}
	defer func() {
		_, _ = s.lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1::int4, 0)`, lockNSBuild)
	}()
	return fn(ctx)
}

// collect drops what dead test processes left behind (see the package
// comment): per-test databases and template builds whose owner holds no owner
// lock. It runs under the build lock.
func (s *server) collect(ctx context.Context) error {
	live, err := s.liveOwners(ctx)
	if err != nil {
		return err
	}
	names, err := s.databases(ctx, clonePrefix)
	if err != nil {
		return err
	}
	byOwner := map[string][]string{}
	for _, n := range names {
		if m := cloneName.FindStringSubmatch(n); m != nil {
			byOwner[m[1]] = append(byOwner[m[1]], n)
		}
	}
	builds, err := s.databases(ctx, buildPrefix)
	if err != nil {
		return err
	}
	for _, n := range builds {
		if m := buildName.FindStringSubmatch(n); m != nil {
			byOwner[m[1]] = append(byOwner[m[1]], n)
		}
	}
	for owner, dbs := range byOwner {
		if err := s.collectOwner(ctx, owner, dbs, live); err != nil {
			return err
		}
	}
	return nil
}

// liveOwners returns the owner ids (hex) whose owner lock some session holds,
// in ANY database of the server: pg_locks is cluster-wide, so two runs that
// use different maintenance databases still see each other as alive.
func (s *server) liveOwners(ctx context.Context) (map[string]bool, error) {
	rows, err := s.admin.QueryContext(ctx, `SELECT DISTINCT lpad(to_hex(objid::bigint), 8, '0') FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 2 AND classid = $1::bigint::oid`, int64(lockNSOwner))
	if err != nil {
		return nil, fmt.Errorf("list live test processes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	live := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		live[h] = true
	}
	return live, rows.Err()
}

// collectOwner drops dbs, the databases of owner, unless owner is a live
// process. Until this process has started (its first collect, before it
// creates any database), databases under its own id are leftovers of a dead
// process that drew the same id; after that they are its own.
func (s *server) collectOwner(ctx context.Context, owner string, dbs []string, live map[string]bool) error {
	if owner == s.ownerHex() && s.started {
		return nil
	}
	if owner != s.ownerHex() {
		if live[owner] {
			return nil // a live process's databases
		}
		id, err := strconv.ParseUint(owner, 16, 32)
		if err != nil {
			return nil
		}
		key, ok := ownerLockKey(id)
		if !ok {
			return nil // not an id this package draws (owners are 31-bit)
		}
		var free bool
		if err := s.lockConn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1::int4, $2::int4)`, lockNSOwner, key).Scan(&free); err != nil {
			return fmt.Errorf("probe owner %s: %w", owner, err)
		}
		if !free {
			return nil // it came alive since liveOwners
		}
		// Hold the dead owner's lock while dropping, so a new process cannot
		// draw the id meanwhile; then let it go.
		defer func() {
			_, _ = s.lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1::int4, $2::int4)`, lockNSOwner, key)
		}()
	}
	for _, db := range dbs {
		if err := s.drop(ctx, db); err != nil && !isPrivilege(err) { // another role's leftovers are its own to collect
			return err
		}
	}
	return nil
}

// databases lists the databases whose names start with prefix.
func (s *server) databases(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE starts_with(datname, $1) ORDER BY oid`, prefix)
	if err != nil {
		return nil, fmt.Errorf("list %s* databases: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// drop drops a database testdb owns, ending any connection still on it (a
// goroutine that outlived its test).
func (s *server) drop(ctx context.Context, name string) error {
	if !ours(name) {
		return fmt.Errorf("refusing to drop %q: not a testdb database", name)
	}
	if strings.HasPrefix(name, templatePrefix) {
		// A template cannot be dropped until it is unmarked.
		if _, err := s.admin.ExecContext(ctx, `ALTER DATABASE `+quoteIdent(name)+` WITH IS_TEMPLATE false`); err != nil && !isMissing(err) {
			return fmt.Errorf("unmark template %s: %w", name, err)
		}
	}
	if _, err := s.admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop %s: %w", name, err)
	}
	return nil
}

func ours(name string) bool {
	return strings.HasPrefix(name, clonePrefix) || strings.HasPrefix(name, templatePrefix)
}

// dsnFor is the maintenance DSN with its database replaced by name.
func (s *server) dsnFor(name string) string {
	u := *s.base
	u.Path = "/" + name
	u.RawPath = ""
	return u.String()
}

// template returns the name of spec's template, building it when it does not
// exist yet (once per server, under the build lock).
//
// The key folds in the test role: a clone's objects belong to whoever owns
// them in the template, so a template built by another role would hand this
// role a database it cannot write to (a CREATEDB-only role is not a
// superuser). Each role builds and clones its own.
func (s *server) template(ctx context.Context, spec Spec) (string, error) {
	if !specName.MatchString(spec.Name) || spec.Fingerprint == nil || spec.Migrate == nil {
		return "", fmt.Errorf("invalid Spec %q: want a lower-case Name (1-12 letters), Fingerprint and Migrate", spec.Name)
	}
	sum := sha256.Sum256([]byte(recipeVersion + "\x00" + s.role + "\x00" + spec.Fingerprint()))
	key := spec.Name + "_" + hex.EncodeToString(sum[:6])
	name := templatePrefix + key
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready[name] {
		return name, nil
	}
	err := s.withBuildLock(ctx, func(ctx context.Context) error {
		var exists bool
		if err := s.admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
			return fmt.Errorf("look up template %s: %w", name, err)
		}
		if exists {
			// The rename is a build's last required step, so a template
			// under its final name is complete — built by an earlier run or
			// another process.
			return nil
		}
		return s.build(ctx, spec, key, name)
	})
	if err != nil {
		return "", err
	}
	s.ready[name] = true
	return name, nil
}

// build migrates a new database and renames it to template name. It runs
// under the build lock.
//
// Only CREATEDB is needed. The role owns the database it builds, and an owner
// may clone its own database whether or not it is marked a template, so the
// IS_TEMPLATE / ALLOW_CONNECTIONS marks are best effort: they keep strangers
// off the template on servers that let the owner set them (Postgres 15+ does),
// and a server that refuses them (insufficient_privilege) still gets a
// working template — nothing in the suite connects to one.
func (s *server) build(ctx context.Context, spec Spec, key, name string) error {
	tmp := buildPrefix + key + "_" + s.ownerHex()
	if err := s.drop(ctx, tmp); err != nil {
		return err
	}
	if _, err := s.admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(tmp)+` TEMPLATE template0`); err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	fail := func(err error) error {
		_ = s.drop(context.Background(), tmp)
		return err
	}
	if err := spec.Migrate(ctx, s.dsnFor(tmp)); err != nil {
		return fail(fmt.Errorf("migrate template %s: %w", spec.Name, err))
	}
	// CREATE DATABASE … TEMPLATE refuses a source with connections; make sure
	// none is left (and, where allowed, none can come).
	q := quoteIdent(tmp)
	if _, err := s.admin.ExecContext(ctx, `ALTER DATABASE `+q+` WITH ALLOW_CONNECTIONS false`); err != nil && !isPrivilege(err) {
		return fail(fmt.Errorf("seal %s: %w", tmp, err))
	}
	if _, err := s.admin.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, tmp); err != nil {
		return fail(fmt.Errorf("disconnect %s: %w", tmp, err))
	}
	if err := retryInUse(ctx, func() error {
		_, err := s.admin.ExecContext(ctx, `ALTER DATABASE `+q+` RENAME TO `+quoteIdent(name))
		return err
	}); err != nil {
		if isDuplicate(err) {
			// A run whose maintenance DSN names another database (so another
			// build lock) built it meanwhile; use theirs.
			_ = s.drop(context.Background(), tmp)
			return nil
		}
		return fail(fmt.Errorf("rename %s: %w", tmp, err))
	}
	if _, err := s.admin.ExecContext(ctx, `ALTER DATABASE `+quoteIdent(name)+` WITH IS_TEMPLATE true`); err != nil && !isPrivilege(err) {
		return fmt.Errorf("mark template %s: %w", name, err)
	}
	// Keep the newest few templates of this kind; older ones belong to
	// migration sets nobody has run since. Another role's template is not
	// ours to drop (best effort: insufficient_privilege is skipped).
	all, err := s.databases(ctx, templatePrefix+spec.Name+"_")
	if err != nil {
		return err
	}
	for i := 0; i < len(all)-keepTemplates; i++ { // oldest first (by oid)
		if all[i] == name {
			continue
		}
		if err := s.drop(ctx, all[i]); err != nil && !isPrivilege(err) {
			return err
		}
	}
	return nil
}

// retryInUse retries fn while Postgres reports the database in use (55006),
// for at most ~10 s: a terminated backend takes a moment to go away.
func retryInUse(ctx context.Context, fn func() error) error {
	var err error
	for range 100 {
		if err = fn(); err == nil || !isInUse(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
	return err
}

// createDB creates database name as a clone of template (template0 when
// template is "").
func (s *server) createDB(ctx context.Context, name, template string) error {
	src := template
	if src == "" {
		src = "template0"
	}
	stmt := `CREATE DATABASE ` + quoteIdent(name) + ` TEMPLATE ` + quoteIdent(src) //nolint:gosec // G202: both names are testdb-generated identifiers, quoted
	if s.strategy != "" {
		stmt += ` STRATEGY ` + s.strategy
	}
	return retryInUse(ctx, func() error { _, err := s.admin.ExecContext(ctx, stmt); return err })
}

// create makes a new per-test database (a clone of template, or empty when
// template is "") and drops it when the test ends.
func (s *server) create(ctx context.Context, t testing.TB, template string) (string, error) {
	name := s.reserve(t)
	if err := s.createDB(ctx, name, template); err != nil {
		return "", err
	}
	return name, nil
}

// newName allocates a per-test database name (fleet_t_<owner>_<n>).
func (s *server) newName() string {
	return fmt.Sprintf("%s%s_%d", clonePrefix, s.ownerHex(), s.seq.Add(1))
}

// reserve allocates a per-test database name and registers its drop.
func (s *server) reserve(t testing.TB) string {
	name := s.newName()
	s.dropAtCleanup(t, name)
	return name
}

// dropAtCleanup drops name when the test ends: synchronously, or — under Run —
// on a background dropper that Run waits for before the process exits.
func (s *server) dropAtCleanup(t testing.TB, name string) {
	t.Cleanup(func() {
		if s.bg.on.Load() {
			s.dropLater(name)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.drop(ctx, name); err != nil {
			t.Errorf("testdb: %v", err)
		}
	})
}

// New returns the DSN of a fresh database cloned from spec's template (built
// on first use), dropped when the test ends. The DSN is the maintenance DSN
// with the database replaced; it names no schema.
func New(t testing.TB, spec Spec) string {
	t.Helper()
	s := get(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmpl, err := s.template(ctx, spec)
	if err != nil {
		t.Fatalf("testdb: template %s: %v", spec.Name, err)
	}
	if name, ok := s.pooled(tmpl); ok {
		s.dropAtCleanup(t, name)
		return s.dsnFor(name)
	}
	name, err := s.create(ctx, t, tmpl)
	if err != nil && isMissing(err) {
		// Garbage-collected by a run on another migration set since this
		// process built or found it: build it again.
		s.mu.Lock()
		delete(s.ready, tmpl)
		s.mu.Unlock()
		if tmpl, err = s.template(ctx, spec); err == nil {
			name, err = s.create(ctx, t, tmpl)
		}
	}
	if err != nil {
		t.Fatalf("testdb: clone %s: %v", tmpl, err)
	}
	return s.dsnFor(name)
}

// Empty returns the DSN of a fresh, empty database (created from template0),
// dropped when the test ends.
func Empty(t testing.TB) string {
	t.Helper()
	s := get(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name, err := s.create(ctx, t, "")
	if err != nil {
		t.Fatalf("testdb: create empty database: %v", err)
	}
	return s.dsnFor(name)
}

// MaintenanceDSN returns FLEET_TEST_DATABASE_URL, skipping the test when it is
// unset — for a test that needs a superuser-ish connection to the test server
// itself (to inspect pg_database, or to create a role). Do not write to the
// database it names.
func MaintenanceDSN(t testing.TB) string {
	t.Helper()
	get(t)
	return strings.TrimSpace(envreg.Get(EnvVar))
}

func isInUse(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55006" // object_in_use
}

// isPrivilege reports insufficient_privilege (42501) — another role's
// database, or a setting this role may not change.
func isPrivilege(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42501"
}

func isDuplicate(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42P04" // duplicate_database
}

func isMissing(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "3D000" // invalid_catalog_name
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
