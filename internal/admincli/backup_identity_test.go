package admincli

import (
	"bytes"
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

func TestParseCopyColumn(t *testing.T) {
	t.Parallel()
	script := "SET x = 1;\n" +
		"COPY sched.tasks (id, prompt, created_by) FROM stdin;\n" +
		"1\thello\taaa\n2\tworld\t\\N\n3\tagain\tbbb\n\\.\n\n" +
		"COPY sched.other (id, created_by) FROM stdin;\n9\tzzz\n\\.\n"
	got, err := parseCopyColumn(strings.NewReader(script), "sched.tasks", "created_by")
	if err != nil || len(got) != 2 || !got["aaa"] || !got["bbb"] {
		t.Fatalf("parseCopyColumn = %v, %v; want {aaa, bbb}", got, err)
	}
	// A table without the column (an older schema) gives nothing.
	got, err = parseCopyColumn(strings.NewReader("COPY sched.tasks (id) FROM stdin;\n1\n\\.\n"), "sched.tasks", "created_by")
	if err != nil || len(got) != 0 {
		t.Fatalf("no column: %v, %v", got, err)
	}
}

// No store restore leaves task creators naming no account without the
// operator saying so (Codex 4504789447 on #2083): a sched dump restored into
// an empty database is refused before anything changes (the accounts come
// with chat's dump, whose migrations own core); chat then sched from the same
// backup gives working accounts and resolvable creators; a sched dump into a
// database with other accounts, or a chat dump under a live sched store
// whose creators it lacks, is refused unless --allow-unresolved-creators.
func TestStoreRestoreKeepsTaskCreatorsResolvable(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not found", b)
		}
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", testdb.MaintenanceDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if cm, sm, ok := pgMajorVersions(ctx, admin); ok && cm != sm {
		t.Skipf("pg_dump major %d != server major %d", cm, sm)
	}

	// The source fleet: an Operations Center account and a task it created.
	src := fleetdb.New(t)
	srcSched := src.SchedDB(t)
	acct, err := srcSched.Identity().CreateAccount(ctx, identity.NewAccount{Email: "ops@x.com", Role: identity.RoleViewer, OpsScope: identity.OpsRun})
	if err != nil {
		t.Fatal(err)
	}
	creator := acct.ID
	taskID := uuid.New()
	if err := srcSched.AddTask(ctx, &models.Task{ID: taskID, Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), CreatedBy: &creator}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	chatDump, schedDump := filepath.Join(dir, "chat.dump"), filepath.Join(dir, "sched.dump")
	if err := runPgDump(ctx, src.ChatDSN(), chatDump); err != nil {
		t.Fatal(err)
	}
	if err := runPgDump(ctx, src.SchedDSN(), schedDump); err != nil {
		t.Fatal(err)
	}

	// 1. sched alone into an empty database: refused, nothing created.
	empty := testdb.Empty(t)
	err = runPgRestoreWith(ctx, config.WithSchema(empty, config.SchedSchema), schedDump, restoreOpts{})
	if err == nil || !strings.Contains(err.Error(), "no accounts yet") || !strings.Contains(err.Error(), "--db=all") {
		t.Fatalf("sched restore into an empty database = %v, want a refusal naming chat-first and --db=all", err)
	}
	var n int
	if err := sqlOne(t, empty, `SELECT count(*) FROM pg_namespace WHERE nspname IN ('sched', 'core')`, &n); n != 0 || err != nil {
		t.Fatalf("a refused restore created %d schema(s) (%v)", n, err)
	}

	// 2. chat, then sched, from the same backup: the accounts and the task
	// creator resolve, and both stores open (migrate) on the result.
	var notes bytes.Buffer
	if err := runPgRestoreWith(ctx, config.WithSchema(empty, config.ChatSchema), chatDump, restoreOpts{notes: &notes}); err != nil {
		t.Fatalf("chat restore: %v", err)
	}
	if err := runPgRestoreWith(ctx, config.WithSchema(empty, config.SchedSchema), schedDump, restoreOpts{notes: &notes}); err != nil {
		t.Fatalf("sched restore after chat: %v", err)
	}
	if !strings.Contains(notes.String(), "identities are kept from this database") {
		t.Errorf("notes = %q", notes.String())
	}
	var email string
	if err := sqlOne(t, empty, `SELECT u.email FROM sched.tasks t JOIN core.users u ON u.id = t.created_by WHERE t.id = '`+taskID.String()+`'`, &email); err != nil || email != "ops@x.com" {
		t.Fatalf("restored task creator = %q, %v", email, err)
	}
	chat, err := store.Open(config.WithSchema(empty, config.ChatSchema), fleetdb.ChatPool())
	if err != nil {
		t.Fatalf("chat store on the restored database: %v", err)
	}
	_ = chat.Close()
	st := storage.New()
	if err := st.Initialize(config.WithSchema(empty, config.SchedSchema), fleetdb.SchedPool()); err != nil {
		t.Fatalf("sched store on the restored database: %v", err)
	}
	if u, err := st.GetIdentity(ctx, "ops@x.com"); err != nil || u.ID != creator || !u.HasOpsAccess() {
		t.Fatalf("restored account = %+v, %v", u, err)
	}
	st.Close()

	// 3. sched into a fleet with other accounts: refused; allowed on request.
	other := fleetdb.New(t)
	err = runPgRestoreWith(ctx, other.SchedDSN(), schedDump, restoreOpts{})
	if err == nil || !strings.Contains(err.Error(), "1 task/dataset creator(s)") {
		t.Fatalf("sched restore against other accounts = %v, want a refusal", err)
	}
	if err := runPgRestoreWith(ctx, other.SchedDSN(), schedDump, restoreOpts{allowUnresolved: true}); err != nil {
		t.Fatalf("sched restore with --allow-unresolved-creators: %v", err)
	}

	// 4. A chat dump without the account under a live sched store whose
	// task names it: refused (it would orphan the live creator).
	bare := fleetdb.New(t)
	bareChat := filepath.Join(dir, "bare-chat.dump")
	if err := runPgDump(ctx, bare.ChatDSN(), bareChat); err != nil {
		t.Fatal(err)
	}
	err = runPgRestoreWith(ctx, config.WithSchema(empty, config.ChatSchema), bareChat, restoreOpts{})
	if err == nil || !strings.Contains(err.Error(), "restoring chat") {
		t.Fatalf("chat restore orphaning live creators = %v, want a refusal", err)
	}
}

func sqlOne(t *testing.T, dsn, q string, dest any) error {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.QueryRowContext(context.Background(), q).Scan(dest)
}

// A chat dump taken after the merge carries its "merged" marker in core. Into
// a database whose scheduler accounts are still unmerged (sched.users there),
// it would stop that merge for good, so it is refused before anything
// changes (Codex 4505481863 on #2083).
func TestChatRestoreRefusesToSkipAPendingMerge(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not found", b)
		}
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", testdb.MaintenanceDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if cm, sm, ok := pgMajorVersions(ctx, admin); ok && cm != sm {
		t.Skipf("pg_dump major %d != server major %d", cm, sm)
	}
	src := fleetdb.New(t) // both stores migrated: the merge ran and is recorded
	chatDump := filepath.Join(t.TempDir(), "chat.dump")
	if err := runPgDump(ctx, src.ChatDSN(), chatDump); err != nil {
		t.Fatal(err)
	}

	// The target: the scheduler migrated first, its accounts not merged yet.
	target := testdb.Empty(t)
	st := storage.New()
	if err := st.Initialize(config.WithSchema(target, config.SchedSchema), fleetdb.SchedPool()); err != nil {
		t.Fatal(err)
	}
	if st.DB().IdentityMerge().Ready {
		t.Fatal("the target merged without a chat store")
	}
	st.Close()

	err = runPgRestoreWith(ctx, config.WithSchema(target, config.ChatSchema), chatDump, restoreOpts{allowUnresolved: true})
	if err == nil || !strings.Contains(err.Error(), "unmerged scheduler accounts") || !strings.Contains(err.Error(), "--db=all") {
		t.Fatalf("chat restore over a pending merge = %v, want a refusal", err)
	}
	var n int
	if err := sqlOne(t, target, `SELECT count(*) FROM pg_namespace WHERE nspname IN ('chat', 'core')`, &n); err != nil || n != 0 {
		t.Fatalf("a refused restore created %d schema(s) (%v)", n, err)
	}
}
