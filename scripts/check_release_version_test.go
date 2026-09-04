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
		{"scripts/version.sh released-at", "the idempotence check that stops a re-run opening a second ordinal — and it must be the STRICT one, since a v2026.09.04.1oops tag matching the glob would otherwise read as \"already released\""},
		{"gh release view", "publication must be idempotent: a re-run after a failed `gh release create` is how a tag without its release is recovered"},
		{"gh release create", "the tag is published as a release so its notes are generated"},
		{"group: release-${{ github.event.workflow_run.head_sha }}", "the concurrency group must key on the COMMIT: a single `release` group displaces the PENDING run when a third queues, dropping a green commit's tag entirely"},
	} {
		if !strings.Contains(wf, want.needle) {
			t.Errorf(".github/workflows/release.yml no longer contains %q — %s", want.needle, want.why)
		}
	}

	// The tag must land on the exact SHA CI certified, not on whatever `main`
	// points at by the time this job runs.
	if !strings.Contains(wf, "workflow_run.head_sha") {
		t.Errorf("release.yml does not tag github.event.workflow_run.head_sha; tagging a moved main would name a tree CI never ran")
	}
	// The already-tagged path must still hand the tag to the publish step. If it
	// exits without outputs, a tag whose `gh release create` failed once can
	// never be published by a re-run.
	if !strings.Contains(wf, "echo \"released=true\"") {
		t.Errorf("release.yml's already-tagged path does not set the `released` output; a tag whose publication failed would stay unpublished forever (only a human could fix it)")
	}
	if !strings.Contains(wf, "merge-base --is-ancestor") {
		t.Errorf("release.yml does not prove the certified SHA is on the checked-out branch before tagging it; a rewritten main would get a release tag on an orphaned commit")
	}

	// The untrusted-checkout boundary, which is easy to "helpfully" undo.
	//
	// This job holds a `contents: write` token on a workflow_run trigger and
	// then EXECUTES scripts/version.sh from the checkout, so a `ref:` taken from
	// the event payload turns the checkout into a privilege-escalation sink —
	// CodeQL's actions/untrusted-checkout/high and Semgrep's
	// workflow-run-target-code-checkout both fail the build on it, which is how
	// this was caught. The certified SHA reaches git as a tag TARGET only; the
	// code that runs is the default branch's. Re-adding the ref would look like
	// a tidy-up ("check out what we're tagging") and would be the bug.
	for _, sink := range []string{
		"ref: ${{ github.event.workflow_run.head_sha }}",
		"ref: ${{ github.event.workflow_run.head_branch }}",
	} {
		if strings.Contains(wf, sink) {
			t.Errorf("release.yml checks out %q. This job runs privileged and executes code from the checkout — take the SHA as a tag target, not a checkout ref (ADR-0059).", sink)
		}
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

// TestDeprecationWindowsAreNotKeyedToAReleaseNumber — a deprecation keyed to a
// release NUMBER can never come due on a date-based train. That is not
// hypothetical: "removed in the first release after 1.0.0" was restated across
// eight operator-facing files and armed in none of them, because no 1.0.0 was
// ever going to be cut (ADR-0059 re-anchored it to a date; ADR-0060 then removed
// the thing it guarded). A window must name a DATE — "the first release on or
// after YYYY-MM-DD" — so this refuses the numbered form's return.
//
// The list is the operator-facing surfaces, named explicitly rather than walked:
// a walk would also sweep up docs/adr/** and CHANGELOG.md, which QUOTE the old
// phrase as history and must keep doing so. History is not drift.
func TestDeprecationWindowsAreNotKeyedToAReleaseNumber(t *testing.T) {
	root := repoRoot(t)

	// `\s+` rather than a literal space: these are prose sentences in
	// hard-wrapped Markdown and Go comments, so the phrase legitimately breaks
	// across lines ("...the first\nrelease after 1.0.0").
	numbered := regexp.MustCompile(`(?is)(first|next)\s+release\s+(after|following)\s+v?\d+\.\d+`)

	for _, rel := range []string{
		"README.md",
		"AGENTS.md",
		"CONTRIBUTING.md",
		"ONBOARDING.md",
		"SECURITY.md",
		"docs/DEPLOYMENT.md",
		"docs/OPERATORS.md",
		"docs/BACKUP_RESTORE.md",
		"docs/VERSIONING.md",
		"Makefile",
	} {
		body := readFile(t, root, rel)
		if m := numbered.FindString(body); m != "" {
			t.Errorf("%s keys a deprecation to a release NUMBER (%q). No numbered release will ever be cut — date the window instead: \"the first release on or after YYYY-MM-DD\" (ADR-0059).", rel, m)
		}
	}
}

// TestTheFleetAdminShimIsGone — ADR-0060 removed it, and the removal has two
// halves that are easy to half-do. The repo half is this: no package to build,
// and nothing in the build/install path that would resurrect it. (The other half
// — evicting the copy already installed on a box — lives in scripts/update.sh
// and scripts/doctor.sh, which is why the strings below must stay there.)
func TestTheFleetAdminShimIsGone(t *testing.T) {
	root := repoRoot(t)

	if _, err := os.Stat(filepath.Join(root, "cmd", "fleet-admin")); err == nil {
		t.Errorf("cmd/fleet-admin is back. The operator CLI is `fleet <verb>` and has been since #461; the shim was removed in ADR-0060.")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat cmd/fleet-admin: %v", err)
	}

	mk := readFile(t, root, "Makefile")
	if strings.Contains(mk, "fleet-admin ./cmd/fleet-admin") || strings.Contains(mk, "/fleet-admin\"") {
		t.Errorf("the Makefile builds or installs fleet-admin again (ADR-0060 removed it)")
	}

	// A box that already has the shim keeps a stale, never-rebuilt operator CLI
	// on PATH until something deletes it. Both convergence paths must.
	for _, rel := range []string{"scripts/update.sh", "scripts/doctor.sh"} {
		body := readFile(t, root, rel)
		if !strings.Contains(body, "/usr/local/bin/fleet-admin") {
			t.Errorf("%s no longer evicts a leftover fleet-admin shim. Removing it from the repo does not remove it from a box — it stays on PATH running whatever code it was built from (ADR-0060).", rel)
		}
	}
}
