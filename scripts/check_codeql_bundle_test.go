// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// codeql.yml analyzes with the bundle pinned in .github/codeql-bundle.lock
// (cache first, then a verified download) instead of letting
// github/codeql-action/init download one, so a GitHub release-asset outage no
// longer fails every leg and, through `CI gate`, every PR. These tests hold the
// lock to the action it was taken from and to the matrix that reads it.

type codeqlLock struct {
	action, version string
	langs           map[string]string
}

func loadCodeQLLock(t *testing.T) codeqlLock {
	t.Helper()
	l := codeqlLock{langs: map[string]string{}}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, line := range strings.Split(readFile(t, repoRoot(t), ".github/codeql-bundle.lock"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("codeql-bundle.lock: %q is not `key value`", line)
		}
		switch f[0] {
		case "action":
			l.action = f[1]
		case "version":
			l.version = f[1]
		default:
			if !sha.MatchString(f[1]) {
				t.Errorf("codeql-bundle.lock: %s's SHA-256 %q is not 64 lowercase hex digits", f[0], f[1])
			}
			l.langs[f[0]] = f[1]
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(l.action) {
		t.Errorf("codeql-bundle.lock: action %q is not a commit SHA", l.action)
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(l.version) {
		t.Errorf("codeql-bundle.lock: version %q is not a plain CLI version", l.version)
	}
	return l
}

// TestCodeQLBundleLockMatchesThePinnedAction — the bundle version is what the
// pinned action's src/defaults.json names (update-pins.sh derives it from
// exactly that commit), so the lock must name the commit every
// github/codeql-action step in codeql.yml runs. A Dependabot bump of the action
// fails here until `PINS_ONLY=CODEQL_BUNDLE scripts/update-pins.sh` moves the
// lock with it; the scan never silently runs a bundle the action did not ship
// with.
func TestCodeQLBundleLockMatchesThePinnedAction(t *testing.T) {
	lock := loadCodeQLLock(t)
	wf := readFile(t, repoRoot(t), ".github/workflows/codeql.yml")
	refs := regexp.MustCompile(`github/codeql-action/([a-z-]+)@([0-9a-f]{40})`).FindAllStringSubmatch(wf, -1)
	if len(refs) == 0 {
		t.Fatal("codeql.yml pins no github/codeql-action step by commit SHA")
	}
	for _, m := range refs {
		if m[2] != lock.action {
			t.Errorf("codeql.yml runs github/codeql-action/%s@%s but .github/codeql-bundle.lock was taken from %s — "+
				"run `PINS_ONLY=CODEQL_BUNDLE scripts/update-pins.sh` and commit the lock", m[1], m[2], lock.action)
		}
	}
	p, ok := loadPins(t)["CODEQL_BUNDLE"]
	if !ok || p.kind != "codeql" {
		t.Error("scripts/lib/pins.tsv has no `CODEQL_BUNDLE codeql` row, so nothing moves .github/codeql-bundle.lock")
	}
}

// TestCodeQLWorkflowUsesThePinnedBundle — every matrix leg names a bundle the
// lock pins (and the lock pins nothing unused), the bundle goes through the
// cache and the verifying installer, and init is handed it via `tools:`.
func TestCodeQLWorkflowUsesThePinnedBundle(t *testing.T) {
	lock := loadCodeQLLock(t)
	wf := readFile(t, repoRoot(t), ".github/workflows/codeql.yml")
	var doc struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []map[string]string `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Name string            `yaml:"name"`
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(wf), &doc); err != nil {
		t.Fatal(err)
	}
	job, ok := doc.Jobs["analyze"]
	if !ok {
		t.Fatal("codeql.yml has no analyze job")
	}
	used := map[string]bool{}
	for _, leg := range job.Strategy.Matrix.Include {
		b := leg["bundle"]
		if _, ok := lock.langs[b]; !ok {
			t.Errorf("codeql.yml leg %q uses bundle %q, which .github/codeql-bundle.lock does not pin", leg["language"], b)
		}
		used[b] = true
	}
	var unused []string
	for l := range lock.langs {
		if !used[l] {
			unused = append(unused, l)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Errorf(".github/codeql-bundle.lock pins bundles no leg uses: %v", unused)
	}
	var cached, installed, toolsSet bool
	for _, s := range job.Steps {
		switch {
		case strings.HasPrefix(s.Uses, "actions/cache@"):
			key := s.With["key"]
			cached = strings.Contains(key, "steps.bundle.outputs.version") && strings.Contains(key, "steps.bundle.outputs.sha256")
		case strings.Contains(s.Run, "scripts/install-pinned-tool.sh"):
			installed = strings.Contains(s.Run, "--sha256") && strings.Contains(s.Run, "fleet-pinned-tools/codeql-bundle-")
		case strings.HasPrefix(s.Uses, "github/codeql-action/init@"):
			toolsSet = strings.Contains(s.With["tools"], "codeql-bundle/${{ steps.bundle.outputs.asset }}")
		}
	}
	if !cached {
		t.Error("codeql.yml must cache the bundle with actions/cache keyed by the pinned version and SHA-256")
	}
	if !installed {
		t.Error("codeql.yml must install the bundle with scripts/install-pinned-tool.sh (cache, then a verified download)")
	}
	if !toolsSet {
		t.Error("codeql.yml's init step must pass the verified local bundle as `tools:`; without it init downloads its own")
	}
}
