// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ci.yml and dev-ci.yml each end in an aggregate gate job (`CI gate` /
// `Dev gate`) whose `needs` is the list branch protection is pointed at. A job
// that runs but is NOT in that list is red-but-not-required — it can fail
// forever behind a green gate, which is exactly how the CodeQL Go extraction
// break sat unnoticed for weeks and the reason #1246 was written.
//
// Nothing asserted the list was complete. Adding a job and forgetting to extend
// `needs` is a silent, one-line regression with no failing test, so this is that
// test: every job in the file except the gate itself must be in the gate's
// needs.
//
// Deliberately a hand-rolled scan rather than a YAML dependency: the `scripts`
// package has no non-test Go files and no imports beyond the standard library,
// and adding a YAML parser to the module for one assertion is a worse trade than
// a regexp over a file whose shape this repo controls.

var (
	// A top-level job key: exactly two spaces of indent, then `name:`.
	jobKeyRe = regexp.MustCompile(`(?m)^  ([a-zA-Z0-9_-]+):$`)
	needsRe  = regexp.MustCompile(`(?m)^    needs:\s*\[([^\]]*)\]`)
)

func TestAggregateGateNeedsEveryJob(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct{ file, gate string }{
		{"ci.yml", "ci-gate"},
		{"dev-ci.yml", "dev-gate"},
	} {
		raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		text := string(raw)

		// Only look below `jobs:` so a top-level key like `permissions:` is not
		// mistaken for a job.
		jobsAt := strings.Index(text, "\njobs:\n")
		if jobsAt < 0 {
			t.Fatalf("%s: no top-level `jobs:` block", tc.file)
		}
		jobsBlock := text[jobsAt:]

		matches := jobKeyRe.FindAllStringSubmatch(jobsBlock, -1)
		jobs := make([]string, 0, len(matches))
		for _, m := range matches {
			jobs = append(jobs, m[1])
		}
		if len(jobs) < 2 {
			t.Fatalf("%s: found %d jobs — the scan is broken, not the workflow", tc.file, len(jobs))
		}

		gateAt := strings.Index(jobsBlock, "\n  "+tc.gate+":\n")
		if gateAt < 0 {
			t.Fatalf("%s: no `%s` job — if the gate was renamed, update this test AND the branch ruleset", tc.file, tc.gate)
		}
		m := needsRe.FindStringSubmatch(jobsBlock[gateAt:])
		if m == nil {
			t.Fatalf("%s: `%s` has no inline `needs: [...]` — this test only understands the inline form", tc.file, tc.gate)
		}
		needs := map[string]bool{}
		for _, n := range strings.Split(m[1], ",") {
			if n = strings.TrimSpace(n); n != "" {
				needs[n] = true
			}
		}

		var missing []string
		for _, j := range jobs {
			if j == tc.gate {
				continue
			}
			if !needs[j] {
				missing = append(missing, j)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: job(s) %v run but are not in `%s`'s needs — they are red-but-not-required. Add them, or the gate reports green while they fail.",
				tc.file, missing, tc.gate)
		}
		t.Logf("%s: %s covers %d/%d jobs", tc.file, tc.gate, len(needs), len(jobs)-1)
	}
}
