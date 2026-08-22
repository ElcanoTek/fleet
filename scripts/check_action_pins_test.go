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

// Every third-party `uses:` in .github/workflows must be pinned to a 40-hex
// commit SHA with a trailing `# vX.Y.Z` comment. Two separate properties, and
// the repo has been bitten by the second one:
//
//  1. A mutable tag (`@v4`) lets whoever controls the upstream repository change
//     what fleet's CI executes, which is the whole reason #1246 converted 53
//     refs to SHAs.
//  2. The comment has to name an EXACT version. Two refs shipped as
//     `# v4 (4.37.8)` and `# v9`, and dependabot-core parses the leading version
//     token — so it read those as "4" and "9". Worse, both SHAs were the
//     ANNOTATED TAG OBJECT of the mutable major tag rather than the commit it
//     pointed at (verified with `git ls-remote --tags`: `refs/tags/v4` ->
//     4c0873ef, `refs/tags/v4^{}` -> db488dde). A tag object is immutable, but
//     it is only reachable while that tag still points at it — the moment
//     upstream moves `v4`, the object is unreferenced and Actions can no longer
//     resolve the ref. That is a self-inflicted CI outage with no bad actor
//     involved, and it was armed in six places.
//
// This test cannot tell a tag object from a commit offline (that needs the
// network). It enforces the shape, which is what makes the drift reviewable:
// an exact version comment is what lets a human or Dependabot check the SHA
// against a release.

var (
	usesLine = regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)\s*(#.*)?$`)
	shaRef   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	exactVer = regexp.MustCompile(`^#\s*v\d+\.\d+\.\d+\b`)
)

func TestWorkflowsPinActionsBySHAWithExactVersionComment(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range usesLine.FindAllStringSubmatch(string(raw), -1) {
			ref, comment := m[1], strings.TrimSpace(m[2])
			// A local reusable-workflow call is a path, not a versioned action;
			// it is this repository's own code at this repository's own commit.
			if strings.HasPrefix(ref, "./") {
				continue
			}
			at := strings.LastIndex(ref, "@")
			if at < 0 {
				t.Errorf("%s: `uses: %s` has no version at all", e.Name(), ref)
				continue
			}
			checked++
			action, version := ref[:at], ref[at+1:]
			if !shaRef.MatchString(version) {
				t.Errorf("%s: `uses: %s` is pinned to %q, not a 40-hex commit SHA — a mutable ref lets upstream change what CI runs",
					e.Name(), action, version)
				continue
			}
			if comment == "" {
				t.Errorf("%s: `uses: %s@%s` has no version comment — add `# vX.Y.Z` so the pin is reviewable and Dependabot can bump it",
					e.Name(), action, version[:8])
				continue
			}
			if !exactVer.MatchString(comment) {
				t.Errorf("%s: `uses: %s@%s` is commented %q — must name an EXACT version (`# vX.Y.Z`); a bare major reads as that major to Dependabot and hides which release the SHA is",
					e.Name(), action, version[:8], comment)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no third-party `uses:` refs found — this test would pass vacuously")
	}
	t.Logf("checked %d third-party action references", checked)
}
