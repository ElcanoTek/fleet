// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sharedDBPatterns are the ways a test used to reach a database it shared
// with other tests: the old per-store DSN variables read directly, the
// cross-package advisory lock, the TRUNCATE fixture, and the httpapi lane's
// third database. Every DB test now gets a database of its own from
// internal/testdb (#2049 M2), which is the only code allowed to read the
// maintenance DSN; a test that reintroduces one of these would share state
// with whatever runs beside it — and the whole tree now runs in parallel.
var sharedDBPatterns = []string{
	`os.Getenv("DATABASE_URL")`,
	`"FLEET_TEST_DATABASE_URL"`,
	`pg_advisory_lock(1)`,
	`TruncateAllForTest`,
	`HTTPAPI_TEST_DATABASE_URL`,
}

// TestNoTestSharesADatabase is the guard for the parallel suite: no _test.go
// file outside internal/testdb reads the DSN variables or uses a shared-DB
// device; DB tests go through internal/testdb (or internal/testdb/fleetdb).
func TestNoTestSharesADatabase(t *testing.T) {
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "web", ".git", "node_modules":
				return filepath.SkipDir
			}
			if rel, _ := filepath.Rel(root, path); filepath.ToSlash(rel) == "internal/testdb" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "go_test_script_test.go" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, p := range sharedDBUses(string(body)) {
			t.Errorf("%s uses %s: get a database of the test's own from internal/testdb (docs/TESTING.md)", rel, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDBPackagesRunInBackgroundMode: a package whose tests get databases from
// internal/testdb has a TestMain that calls testdb.Run, so its clones are
// made ahead and dropped off the critical path — and none outlives the run.
// Without it the package still works (synchronous clones), just slower,
// which is exactly the kind of regression nobody notices.
func TestDBPackagesRunInBackgroundMode(t *testing.T) {
	root := repoRoot(t)
	uses, runs := map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "web", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		if rel == "internal/testdb" || rel == "scripts" {
			return nil // testdb tests its own modes; scripts only names them
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		if strings.Contains(text, `"github.com/ElcanoTek/fleet/internal/testdb"`) || strings.Contains(text, `"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"`) {
			uses[rel] = true
		}
		if strings.Contains(text, "testdb.Run(m)") {
			runs[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) == 0 {
		t.Fatal("no package uses internal/testdb — the walk is broken")
	}
	for dir := range uses {
		if !runs[dir] {
			t.Errorf("%s gets test databases from internal/testdb but has no TestMain calling testdb.Run(m)", dir)
		}
	}
}

// sharedDBUses returns the sharedDBPatterns text contains.
func sharedDBUses(text string) []string {
	var out []string
	for _, p := range sharedDBPatterns {
		if strings.Contains(text, p) {
			out = append(out, p)
		}
	}
	return out
}

func TestSharedDBUsesRecognisesEachPattern(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{`dsn := os.Getenv("DATABASE_URL")`, 1},
		{`base := os.Getenv("FLEET_TEST_DATABASE_URL")`, 1},
		{`conn.Exec("SELECT pg_advisory_lock(1)")`, 1},
		{`s.TruncateAllForTest(ctx)`, 1},
		{`export HTTPAPI_TEST_DATABASE_URL=x`, 1},
		{`// Skips when FLEET_TEST_DATABASE_URL is unset (internal/testdb).`, 0},
		{`dsn := fleetdb.New(t).ChatDSN()`, 0},
	} {
		if got := sharedDBUses(tc.text); len(got) != tc.want {
			t.Errorf("sharedDBUses(%q) = %v, want %d match(es)", tc.text, got, tc.want)
		}
	}
}

// TestGoTestScriptIsOneRun: with no shared databases there is nothing to
// partition, so the script must not grow DSN groups, -p 1 lanes or a
// prebuild step back.
func TestGoTestScriptIsOneRun(t *testing.T) {
	body := readFile(t, repoRoot(t), "scripts/go-test.sh")
	for _, banned := range []string{"-p 1", "classify", "--print-groups", "HTTPAPI_TEST_DATABASE_URL", "go test -c"} {
		if strings.Contains(body, banned) {
			t.Errorf("go-test.sh contains %q; it is one go test run at default -p (#2049 M2)", banned)
		}
	}
	if strings.Count(body, "go test \"${TAGS[@]}\"") != 1 {
		t.Error("go-test.sh must invoke go test exactly once")
	}
}

// TestGoTestScriptIsWhatCIRuns keeps the CI==local promise: a contributor's
// `make test` / `make test-race` / `make test-cover` is the same script CI
// invokes, so a script bug shows up locally instead of only on the runner.
func TestGoTestScriptIsWhatCIRuns(t *testing.T) {
	root := repoRoot(t)
	ci := readFile(t, root, ".github/workflows/ci.yml")
	makefile := readFile(t, root, "Makefile")

	for _, needle := range []string{
		"scripts/go-test.sh --coverprofile=coverage.out --count=1",
		"scripts/go-test.sh --race --count=1",
	} {
		if !strings.Contains(ci, needle) {
			t.Errorf("ci.yml does not invoke %q", needle)
		}
	}
	for _, needle := range []string{
		"scripts/go-test.sh",
		"scripts/go-test.sh --race",
		"scripts/go-test.sh --coverprofile=coverage.out",
	} {
		if !strings.Contains(makefile, needle) {
			t.Errorf("Makefile does not invoke %q", needle)
		}
	}
}

// TestGoTestScriptSurfacesGoListFailure: mapfile from a process substitution
// keeps a partial package list and discards go list's nonzero status, so the
// suite could pass over an incomplete graph. The script must capture the exit
// code.
func TestGoTestScriptSurfacesGoListFailure(t *testing.T) {
	body := readFile(t, repoRoot(t), "scripts/go-test.sh")
	if strings.Contains(body, "mapfile -t ALL < <(go list") {
		t.Error("go list is in a process substitution; its exit status is discarded")
	}
	if !strings.Contains(body, `if ! go list`) {
		t.Error("go-test.sh does not check go list's exit status")
	}
}
