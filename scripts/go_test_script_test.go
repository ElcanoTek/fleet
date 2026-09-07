// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func goTestPrintGroups(t *testing.T, root string) (classified map[string]string, groupCount map[string]int) {
	t.Helper()
	out, err := exec.Command(filepath.Join(root, "scripts", "go-test.sh"), "--print-groups").CombinedOutput()
	if err != nil {
		t.Fatalf("go-test.sh --print-groups: %v\n%s", err, out)
	}
	classified = map[string]string{}
	groupCount = map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		group, pkg, ok := strings.Cut(line, "\t")
		if !ok || group == "" || pkg == "" {
			t.Fatalf("malformed --print-groups line %q", line)
		}
		if prev, dup := classified[pkg]; dup {
			t.Errorf("package %s classified as both %s and %s", pkg, prev, group)
		}
		classified[pkg] = group
		groupCount[group]++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return classified, groupCount
}

func goListAll(t *testing.T, root string) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "list", "-tags", "fleet_host_executor", "./...")
	cmd.Dir = root
	listed, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, listed)
	}
	want := map[string]bool{}
	for _, pkg := range strings.Split(strings.TrimSpace(string(listed)), "\n") {
		if pkg != "" {
			want[pkg] = true
		}
	}
	return want
}

// TestGoTestScriptPartitionsEveryPackage is the load-bearing check for
// scripts/go-test.sh: every `go list` package is in exactly one group.
func TestGoTestScriptPartitionsEveryPackage(t *testing.T) {
	root := repoRoot(t)
	classified, groupCount := goTestPrintGroups(t, root)
	want := goListAll(t, root)

	for pkg := range want {
		if _, ok := classified[pkg]; !ok {
			t.Errorf("go list package %s is missing from go-test.sh --print-groups", pkg)
		}
	}
	for pkg := range classified {
		if !want[pkg] {
			t.Errorf("go-test.sh reports %s but go list does not", pkg)
		}
	}
	for _, g := range []string{"independent", "chat", "sched", "both"} {
		if groupCount[g] == 0 {
			t.Errorf("group %s is empty — the partition or go list is broken", g)
		}
	}
}

// TestGoTestScriptSerializesDSNTests: a new package that starts TRUNCATEing
// fleet_chat_test from the independent group would otherwise pass locally
// and deadlock in CI.
func TestGoTestScriptSerializesDSNTests(t *testing.T) {
	root := repoRoot(t)
	classified, _ := goTestPrintGroups(t, root)
	const module = "github.com/ElcanoTek/fleet"

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
		return checkDSNTestFile(t, root, module, classified, path)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkDSNTestFile(t *testing.T, root, module string, classified map[string]string, path string) error {
	t.Helper()
	if !strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "go_test_script_test.go" {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(body)
	usesChat := strings.Contains(text, "FLEET_TEST_DATABASE_URL") || strings.Contains(text, "CHAT_TEST_DATABASE_URL")
	usesSched := strings.Contains(text, `os.Getenv("DATABASE_URL")`) || strings.Contains(text, "pg_advisory_lock")
	if !usesChat && !usesSched {
		return nil
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return err
	}
	pkg := module + "/" + filepath.ToSlash(rel)
	group, ok := classified[pkg]
	if !ok {
		t.Errorf("%s talks to a shared DSN but its package %s is not in go list ./...", path, pkg)
		return nil
	}
	serial := group == "chat" || group == "sched" || group == "both"
	switch {
	case !serial:
		t.Errorf("%s uses a shared DSN but package %s is in group %s (want chat, sched, or both)", rel, pkg, group)
	case usesChat && group == "sched":
		t.Errorf("%s uses the chat DSN but package %s is in group sched", rel, pkg)
	case usesSched && group == "chat":
		t.Errorf("%s uses the sched DSN but package %s is in group chat", rel, pkg)
	}
	return nil
}

// TestGoTestScriptIsWhatCIRuns keeps the CI==local promise: a contributor's
// `make test` / `make test-race` / `make test-cover` is the same script CI
// invokes, so a partition bug shows up locally instead of only on the runner.
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
