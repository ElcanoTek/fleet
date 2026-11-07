// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realignFixture builds an origin with main = A(tag) → B and a clone checked
// out at B, and serves a "latest release" API naming the tag on A. It returns
// the clone and the environment for a real (non-dry-run) update.sh run that
// gets as far as the checkout step and then stops at the release download
// (nothing is served there) — which is all these tests look at.
func realignFixture(t *testing.T) (clone string, env []string, sha func(string) string) {
	t.Helper()
	const tag = fixtureTag
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir() // git_trust_dir writes --global config: keep it off the runner's
	base := t.TempDir()
	gitEnv := append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	work := filepath.Join(base, "work")
	_ = os.MkdirAll(work, 0o755)
	run(work, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644)
	run(work, "add", ".")
	run(work, "commit", "-q", "-m", "A")
	run(work, "tag", "-a", tag, "-m", tag)
	_ = os.WriteFile(filepath.Join(work, "b"), []byte("b\n"), 0o644)
	run(work, "add", ".")
	run(work, "commit", "-q", "-m", "B")
	origin := filepath.Join(base, "origin.git")
	run(base, "clone", "-q", "--bare", work, origin)
	clone = filepath.Join(base, "clone")
	run(base, "clone", "-q", origin, clone)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
	}))
	t.Cleanup(api.Close)
	dead := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dead.Close)
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	_ = os.WriteFile(envFile, nil, 0o600)
	env = append([]string{}, gitEnv...)
	env = append(env, "SRC_DIR="+clone, "FLEET_RELEASE_LATEST_URL="+api.URL, "FLEET_RELEASE_BASE_URL="+dead.URL,
		"FLEET_COSIGN_URL="+dead.URL, "FLEET_RELEASE_CACHE_DIR="+t.TempDir(), "FLEET_INSTALL_DIR="+t.TempDir(),
		"FLEET_ENV_FILE="+envFile, "FLEET_UPDATE_FORCE=1", "FLEET_UPDATE_YES=1")
	return clone, env, func(ref string) string { return run(clone, "rev-parse", ref) }
}

// TestUpdateVerifiesTheReleaseBeforeMovingTheCheckout — the checkout is
// moved to a release tag (forward, or back past newer commits) only after the
// tag's SIGNED manifest is verified and names the commit the tag resolves to.
// The fixture serves no signed manifest, so the checkout must stay put and
// nothing from the tag may run.
func TestUpdateVerifiesTheReleaseBeforeMovingTheCheckout(t *testing.T) {
	_, env, sha := realignFixture(t)
	before := sha("HEAD")
	if before == sha(fixtureTag+"^{commit}") {
		t.Fatal("fixture: the clone should start past the tag")
	}
	out, err := runScript(t, env, "update.sh", "--yes")
	if err == nil {
		t.Fatalf("an unverifiable release was accepted\n%s", out)
	}
	if got := sha("HEAD"); got != before {
		t.Fatalf("the checkout moved to an unverified release (%s → %s)\n%s", before, got, out)
	}
	for _, want := range []string{"release to install: " + fixtureTag, "could not be verified", "checkout was NOT moved"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestUpdateMovesTheCheckoutOnlyAfterTheManifestCheck pins the order in the
// script itself (the end-to-end move needs a release signed by fleet's
// workflow, which no test can mint): the manifest check precedes both the
// move back and the fast-forward, and the re-exec of the moved update.sh.
func TestUpdateMovesTheCheckoutOnlyAfterTheManifestCheck(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	verify := strings.Index(s, `fleet_release_verify_manifest "$REPO_ROOT" "$RELEASE_TAG" "$after_sha"`)
	for _, later := range []string{`git checkout --quiet -B "$target_branch" "$after_sha"`, `git merge --ff-only --quiet "$after_sha"`, `exec env FLEET_UPDATE_REEXEC=1`} {
		at := strings.Index(s, later)
		if verify < 0 || at < 0 || at < verify {
			t.Errorf("%q (at %d) must come after the manifest check (at %d)", later, at, verify)
		}
	}
}

// TestUpdateRefusesToStrandLocalCommits — moving back is only safe for commits
// origin still has; a local-only commit refuses rather than disappearing.
func TestUpdateRefusesToStrandLocalCommits(t *testing.T) {
	clone, env, sha := realignFixture(t)
	_ = os.WriteFile(filepath.Join(clone, "local"), []byte("x\n"), 0o644)
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "local only"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = clone, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	before := sha("HEAD")
	out, err := runScript(t, env, "update.sh", "--yes")
	if err == nil || !strings.Contains(out, "would strand them") {
		t.Fatalf("want a refusal naming the stranded commits, got err=%v\n%s", err, out)
	}
	if sha("HEAD") != before {
		t.Fatal("the checkout moved despite the refusal")
	}
}

// TestUpdateNoPullRefusesAnotherReleasesTag — --no-pull reinstalls the
// checkout's release; naming another one would mix that release's binary with
// this checkout's content.
func TestUpdateNoPullRefusesAnotherReleasesTag(t *testing.T) {
	_, env, _ := realignFixture(t)
	out, err := runScript(t, env, "update.sh", "--no-pull", "--tag", "v2026.11.01.1")
	if err == nil || !strings.Contains(out, "is not at v2026.11.01.1") {
		t.Fatalf("want a refusal, got err=%v\n%s", err, out)
	}
}

// fixtureTag is the release realignFixture publishes.
const fixtureTag = "v2026.11.01.1"
