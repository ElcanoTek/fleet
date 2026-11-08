// Package scripts holds test-only coverage for the shell linters in this
// directory. There are deliberately no non-test Go files here — `go build`
// ignores the package; `go test ./scripts` (part of `make test`) runs it.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckMigrations drives scripts/check-migrations.sh in its direct-file
// mode against the fixture migrations in testdata/migrationlint. The point
// (#593) is that BOTH spellings of the dangerous DDL are rejected — Postgres
// makes the COLUMN keyword optional, so `ADD c ... NOT NULL` and `DROP c` are
// the same DDL as their ADD COLUMN / DROP COLUMN forms — while the safe
// constraint/DEFAULT/`.down.sql`/opt-out cases still pass.
func TestCheckMigrations(t *testing.T) {
	cases := []struct {
		fixture  string
		wantFail bool
	}{
		// Dangerous — the forms the linter has always rejected.
		{"add_not_null_with_column.sql", true},
		{"drop_with_column.sql", true},
		// Dangerous — the keyword-less forms (#593).
		{"add_not_null_keywordless.sql", true},
		{"drop_keywordless.sql", true},
		{"mixed_clause_keywordless.sql", true},
		// Safe — must not false-positive.
		{"safe.sql", false},
		// Opt-out directive still honored.
		{"allow_dangerous.sql", false},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			out, err := exec.Command("./check-migrations.sh",
				filepath.Join("testdata", "migrationlint", tc.fixture)).CombinedOutput()
			failed := err != nil
			if failed != tc.wantFail {
				t.Errorf("check-migrations.sh %s: failed=%v, want %v\noutput:\n%s",
					tc.fixture, failed, tc.wantFail, out)
			}
		})
	}
}

// TestCheckMigrations_DownMigrationSkipped asserts the rollback path is exempt:
// a `.down.sql` reversal is EXPECTED to drop what its forward half added.
func TestCheckMigrations_DownMigrationSkipped(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("testdata", "migrationlint", "drop_keywordless.sql")
	dst := filepath.Join(dir, "001_fixture.down.sql")
	if err := exec.Command("cp", src, dst).Run(); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	if out, err := exec.Command("./check-migrations.sh", dst).CombinedOutput(); err != nil {
		t.Errorf("a .down.sql must be skipped, got failure:\n%s", out)
	}
}

// TestCheckMigrations_Layout drives the one-directory rules (#2049 M6) over
// the fixtures under testdata/migrationlint/layout/internal/migrations/sql:
// legacy files are frozen, new files come in NNNN_name .up/.down pairs from
// 0200, and every name a new file uses carries its schema.
func TestCheckMigrations_Layout(t *testing.T) {
	dir := filepath.Join("testdata", "migrationlint", "layout", "internal", "migrations", "sql")
	for _, tc := range []struct {
		fixture string
		want    string // "" passes; else a substring of the failure
	}{
		{"0200_ok.up.sql", ""},
		{"0200_ok.down.sql", ""},
		{"0201_unqualified.up.sql", "unqualified name feature_flags"},
		{"0202_unpaired.up.sql", "0202_unpaired.down.sql is missing"},
		{"0199_too_low.up.sql", "start at version 0200"},
		{"0203_Bad-Name.up.sql", "NNNN_name.up.sql"},
		{"0204_unqualified_down.down.sql", "unqualified name t204"},
		{filepath.Join("legacy", "chat", "100_late_addition.sql"), "legacy migrations are frozen"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			out, err := exec.Command("./check-migrations.sh", filepath.Join(dir, tc.fixture)).CombinedOutput()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("want a pass, got:\n%s", out)
			case tc.want != "" && (err == nil || !strings.Contains(string(out), tc.want)):
				t.Errorf("want a failure naming %q, got (err %v):\n%s", tc.want, err, out)
			}
		})
	}
}

// TestCheckMigrations_DiffMode runs the linter the way CI does — against a
// base commit — in a scratch repository: a pure rename into the layout (the
// M6 move itself) is exempt, a rename with edits and an added legacy file are
// refused, and a new well-formed pair passes.
func TestCheckMigrations_DiffMode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	script, err := filepath.Abs("check-migrations.sh")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lint := func() (string, error) {
		cmd := exec.Command("bash", script)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "MIGRATION_LINT_BASE="+git("rev-parse", "base"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	git("init", "-q")
	// A legacy file that drops a column: the linter would flag it if it were
	// ever checked, so its move proves the rename exemption.
	write("internal/store/migrations/001_initial.sql", "CREATE TABLE t (a int, b int);\nALTER TABLE t DROP COLUMN b;\n")
	write("internal/store/migrations/002_more.sql", "CREATE TABLE u (a int);\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("tag", "base")

	if err := os.MkdirAll(filepath.Join(repo, "internal", "migrations", "sql", "legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("mv", "internal/store/migrations", "internal/migrations/sql/legacy/chat")
	git("commit", "-qm", "move")
	if out, err := lint(); err != nil || !strings.Contains(out, "no changed migration files") {
		t.Fatalf("a pure rename must be exempt (err %v):\n%s", err, out)
	}

	write("internal/migrations/sql/0200_add_flags.up.sql", "CREATE TABLE core.flags (name text);\n")
	write("internal/migrations/sql/0200_add_flags.down.sql", "DROP TABLE core.flags;\n")
	git("add", ".")
	git("commit", "-qm", "new pair")
	if out, err := lint(); err != nil {
		t.Fatalf("a well-formed new pair must pass:\n%s", out)
	}

	// Shipped migrations are append-only: once 0200 is at the base, editing
	// it (or renaming it) is refused; a new 0201 pair is fine.
	git("tag", "-f", "base")
	write("internal/migrations/sql/0200_add_flags.up.sql", "CREATE TABLE core.flags (name text, on_by_default boolean);\n")
	write("internal/migrations/sql/0201_more_flags.up.sql", "CREATE TABLE core.more_flags (name text);\n")
	write("internal/migrations/sql/0201_more_flags.down.sql", "DROP TABLE core.more_flags;\n")
	git("add", ".")
	git("commit", "-qm", "edit a shipped migration")
	out, err := lint()
	if err == nil || strings.Count(out, "shipped migrations are append-only") != 1 || !strings.Contains(out, "FAIL: internal/migrations/sql/0200_add_flags.up.sql") {
		t.Fatalf("an edited shipped migration must be refused, and only it (err %v):\n%s", err, out)
	}
	git("reset", "-q", "--hard", "base")
	git("mv", "internal/migrations/sql/0200_add_flags.down.sql", "internal/migrations/sql/0200_add_flags_renamed.down.sql")
	git("commit", "-qm", "rename a shipped migration")
	if out, err := lint(); err == nil || !strings.Contains(out, "shipped migrations are append-only") {
		t.Fatalf("a renamed shipped migration must be refused (err %v):\n%s", err, out)
	}
	git("reset", "-q", "--hard", "base")
	// Deleting shipped migrations — both halves of a pair, or a legacy file —
	// is refused: a database that ran them would refuse to start.
	git("rm", "-q", "internal/migrations/sql/0200_add_flags.up.sql", "internal/migrations/sql/0200_add_flags.down.sql",
		"internal/migrations/sql/legacy/chat/001_initial.sql")
	git("commit", "-qm", "delete shipped migrations")
	if out, err := lint(); err == nil || strings.Count(out, "a migration may not be deleted") != 3 {
		t.Fatalf("deleted migrations must be refused, each (err %v):\n%s", err, out)
	}
	git("reset", "-q", "--hard", "base")
	// A rename within legacy/ is a change: the file name is the migration's
	// recorded name (Codex 4506235349). Only the M6 move itself is exempt.
	git("mv", "internal/migrations/sql/legacy/chat/002_more.sql", "internal/migrations/sql/legacy/chat/002_renamed.sql")
	git("commit", "-qm", "rename a legacy migration")
	if out, err := lint(); err == nil || !strings.Contains(out, "legacy migrations are frozen") {
		t.Fatalf("a rename within legacy/ must be refused (err %v):\n%s", err, out)
	}
	git("reset", "-q", "--hard", "base")
	// A new migration numbered at or below the last shipped one is refused.
	write("internal/migrations/sql/0150_too_early.up.sql", "SELECT 1;\n")
	write("internal/migrations/sql/0150_too_early.down.sql", "SELECT 1;\n")
	write("internal/migrations/sql/0200_add_flags_again.up.sql", "SELECT 1;\n")
	write("internal/migrations/sql/0200_add_flags_again.down.sql", "SELECT 1;\n")
	git("add", ".")
	git("commit", "-qm", "out of order")
	if out, err := lint(); err == nil || strings.Count(out, "must come after the last shipped one") != 4 {
		t.Fatalf("migrations at or below the last shipped version must be refused (err %v):\n%s", err, out)
	}
	git("reset", "-q", "--hard", "base")

	write("internal/migrations/sql/legacy/chat/002_more.sql", "CREATE TABLE u (a int);\nCREATE TABLE v (a int);\n")
	write("internal/migrations/sql/legacy/chat/003_late.sql", "SELECT 1;\n")
	git("add", ".")
	git("commit", "-qm", "edit legacy")
	out, err = lint()
	if err == nil || strings.Count(out, "legacy migrations are frozen") != 2 {
		t.Fatalf("an edited and an added legacy file must both be refused (err %v):\n%s", err, out)
	}
}
