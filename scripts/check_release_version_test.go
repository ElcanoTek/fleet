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

// The release version is DERIVED, never authored: releases are date-based
// (vYYYY.MM.DD.N), tagged automatically on every green push to main, and every
// build stamps what scripts/version.sh reads out of those tags (ADR-0059,
// docs/VERSIONING.md).
//
// The sibling file's rule — "every version gets ONE declaration point" — is what
// these tests apply to the release number, whose declaration point is now the
// tag history rather than a file. That is a rule with no natural enforcement:
// a hand-typed version number in a file compiles, lints, and ships. It rotted
// exactly that way before this refactor — a `VERSION` file stuck at 0.0.0 while
// the Helm chart said 0.1.0, web/package.json said 0.1.0, and the API docs
// illustrated a 1.2.0 that never existed. So the drift is a test failure here
// instead of something a reviewer has to notice.

// TestNoVersionFileAtTheRepoRoot — the file this scheme replaced. Its return
// would reinstate a number someone has to remember to move.
func TestNoVersionFileAtTheRepoRoot(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "VERSION")); err == nil {
		t.Errorf("a top-level VERSION file is back. The release version is derived from the release tags by scripts/version.sh — see docs/VERSIONING.md and ADR-0059.")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat VERSION: %v", err)
	}
}

// TestMakefileStampsFromTheVersionScript — the one stamping path. If the
// Makefile stops asking scripts/version.sh, binaries silently report "dev" (or
// worse, a stale literal) and `fleet version` stops identifying the build.
func TestMakefileStampsFromTheVersionScript(t *testing.T) {
	root := repoRoot(t)
	mk := readFile(t, root, "Makefile")

	if !strings.Contains(mk, "VERSION := $(shell scripts/version.sh describe)") {
		t.Errorf("Makefile does not derive VERSION from `scripts/version.sh describe`; the ldflags stamp is the only thing that gives a binary its identity (ADR-0059)")
	}
	if strings.Contains(mk, "$(file < VERSION)") || strings.Contains(mk, "cat VERSION") {
		t.Errorf("Makefile reads a VERSION file again; the release version comes from the release tags (docs/VERSIONING.md)")
	}
	if !strings.Contains(mk, "-X $(VERSION_PKG).version=$(VERSION)") {
		t.Errorf("Makefile no longer stamps internal/version.version via -ldflags -X; an unstamped build reports the \"dev\" sentinel forever")
	}
}

// TestReleaseWorkflowTagsEveryGreenPushToMain — "nobody ever tags a version" is
// only true while this workflow exists and stays gated. Each assertion below is
// a way the automation could quietly stop being the thing it claims to be:
// releasing red trees, releasing PR heads, or releasing nothing at all.
func TestReleaseWorkflowTagsEveryGreenPushToMain(t *testing.T) {
	root := repoRoot(t)
	wf := readFile(t, root, ".github/workflows/release.yml")

	for _, want := range []struct{ needle, why string }{
		{"workflow_run:", "the release must key off CI's completion, not its own trigger"},
		{"workflows: [CI]", "it must watch the full CI gate lane specifically"},
		{"conclusion == 'success'", "a red CI run must NOT be tagged"},
		{"event == 'push'", "a workflow_dispatch CI run on a PR head must NOT be tagged"},
		{"head_branch == 'main'", "only main releases"},
		{"scripts/version.sh next", "the ordinal must come from the one script that knows the format"},
		{"contents: write", "the job cannot push a tag without it"},
		{"--exact-match", "the idempotence check that stops a re-run opening a second ordinal"},
		{"gh release create", "the tag is published as a release so its notes are generated"},
	} {
		if !strings.Contains(wf, want.needle) {
			t.Errorf(".github/workflows/release.yml no longer contains %q — %s", want.needle, want.why)
		}
	}

	// The tag must be pushed to the exact SHA CI certified, not to whatever
	// `main` points at by the time this job runs.
	if !strings.Contains(wf, "workflow_run.head_sha") {
		t.Errorf("release.yml does not tag github.event.workflow_run.head_sha; tagging a moved main would name a tree CI never ran")
	}
}

// TestNoHandAuthoredReleaseNumbers — the placeholders, and the fact that they
// stay placeholders. Helm and npm both PARSE their version fields, so neither
// can simply be dropped; what they must not become is a second source of truth
// that someone bumps by hand (`make helm-package` stamps the real value from the
// tags at package time).
func TestNoHandAuthoredReleaseNumbers(t *testing.T) {
	root := repoRoot(t)

	chart := readFile(t, root, "deploy/helm/fleet/Chart.yaml")
	for _, want := range []string{"version: 0.0.0", `appVersion: "0.0.0"`} {
		if !strings.Contains(chart, want) {
			t.Errorf("deploy/helm/fleet/Chart.yaml no longer holds the placeholder %q. The chart is not published separately and its version is stamped by `make helm-package` from the release tags (ADR-0059); a hand-typed chart version is the drift this replaced.", want)
		}
	}

	// Both npm packages are private and never published, so their "version" is
	// pure ceremony — pinned at the placeholder, and matching their lockfiles so
	// `npm ci` stays happy.
	for _, pkg := range []string{"web", "scripts/rampart-service"} {
		manifest := pkgJSON(t, root, filepath.Join(pkg, "package.json"))
		got, _ := manifest["version"].(string)
		if got != "0.0.0" {
			t.Errorf("%s/package.json version is %q, want the \"0.0.0\" placeholder: the package is private and unpublished, and fleet's release version lives in the release tags (docs/VERSIONING.md)", pkg, got)
		}
		lock := pkgJSON(t, root, filepath.Join(pkg, "package-lock.json"))
		lockVer, _ := lock["version"].(string)
		if lockVer != got {
			t.Errorf("%s/package-lock.json version is %q but package.json says %q — `npm ci` refuses an out-of-sync pair", pkg, lockVer, got)
		}
	}
}

// TestDeprecationWindowsAreDatedNotNumbered — a deprecation keyed to a release
// NUMBER can never come due on a date-based train, which is how "removed in the
// first release after 1.0.0" ended up restated across eight files and armed in
// none of them. Windows are dates now; this refuses the numbered form's return.
func TestDeprecationWindowsAreDatedNotNumbered(t *testing.T) {
	root := repoRoot(t)

	// `\s+` rather than a literal space throughout: these are prose sentences in
	// hard-wrapped Markdown and Go comments, so the phrase legitimately breaks
	// across lines ("...the first\nrelease on or after 2026-12-01"). A
	// space-literal pattern would report a correctly dated file as undated.
	//
	// "first release after <semver>" — the form that can never come due.
	numbered := regexp.MustCompile(`(?is)first\s+release\s+(after|following)\s+\d+\.\d+`)
	// The dated replacement, so the check is "dated form present", not merely
	// "numbered form absent" — a deleted window is not a fixed one.
	dated := regexp.MustCompile(`(?is)first\s+release\s+on\s+or\s+after\s+\d{4}-\d{2}-\d{2}`)

	// The files that carried the shim's removal trigger. Named explicitly rather
	// than discovered: the point is that these specific operator-facing surfaces
	// agree, and a walk would silently pass if one lost the claim entirely.
	carriers := []string{
		"README.md",
		"AGENTS.md",
		"CONTRIBUTING.md",
		"ONBOARDING.md",
		"docs/DEPLOYMENT.md",
		"docs/OPERATORS.md",
		"docs/BACKUP_RESTORE.md",
		"Makefile",
		"cmd/fleet-admin/main.go",
	}
	for _, rel := range carriers {
		body := readFile(t, root, rel)
		if numbered.MatchString(body) {
			t.Errorf("%s still keys a deprecation to a release NUMBER (%q). No numbered release will ever be cut — use \"the first release on or after YYYY-MM-DD\" (ADR-0059, ADR-0012).", rel, numbered.FindString(body))
		}
		if !dated.MatchString(body) {
			t.Errorf("%s no longer states the fleet-admin shim's dated removal window. Every carrier of that claim must agree, or the deprecation means something different depending on which file you read.", rel)
		}
	}
}
