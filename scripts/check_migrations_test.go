// Package scripts holds test-only coverage for the shell linters in this
// directory. There are deliberately no non-test Go files here — `go build`
// ignores the package; `go test ./scripts` (part of `make test`) runs it.
package scripts

import (
	"os/exec"
	"path/filepath"
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
