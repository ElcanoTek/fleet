// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every workflow in this repo declares a top-level `permissions:` block, and
// that is load-bearing rather than tidy: a workflow WITHOUT one inherits the
// repository default, which can be read-write for every scope. The whole
// least-privilege posture documented in docs/SCANNING.md rests on the property
// holding for all of them, and a new workflow added without the block is a
// silent, invisible regression — there is no failing check, just a job quietly
// holding more token than it asked for.
//
// So assert it, in the same spirit as check_action_pins_test.go and
// check_gate_needs_test.go: the invariants this repo cares about are tests, not
// review habits. The assertion is deliberately only that the block EXISTS — its
// contents are a per-workflow judgement call (`{}`, `contents: read`, or a
// scoped set), and pinning those here would fight every legitimate change.
var topLevelPermissionsRe = regexp.MustCompile(`(?m)^permissions:`)

func TestWorkflowsDeclareTopLevelPermissions(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	seen := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		seen++
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if !topLevelPermissionsRe.Match(raw) {
			t.Errorf("%s: no top-level `permissions:` block — the workflow inherits the "+
				"repository default token scopes. Declare one (`permissions: {}` if the "+
				"workflow needs nothing) and grant writes on the job that needs them.", e.Name())
		}
	}
	if seen == 0 {
		t.Fatal("no workflow files found — this test would pass vacuously")
	}
	t.Logf("checked %d workflow files", seen)
}
