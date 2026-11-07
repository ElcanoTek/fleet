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
func realignFixture(t *testing.T, tag string) (clone string, env []string, sha func(string) string) {
	t.Helper()
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

// TestUpdateMovesACheckoutBackToTheRelease — a checkout past the release (a
// downgrade, or a box that tracked the branch tip) must not keep its newer
// units and scripts next to an older binary: update moves it back to the tag.
func TestUpdateMovesACheckoutBackToTheRelease(t *testing.T) {
	clone, env, sha := realignFixture(t, "v2026.11.01.1")
	tagSHA := sha("v2026.11.01.1^{commit}")
	if sha("HEAD") == tagSHA {
		t.Fatal("fixture: the clone should start past the tag")
	}
	out, err := runScript(t, env, "update.sh", "--yes")
	if err == nil {
		t.Fatalf("the fixture serves no release, so the run must stop at the download\n%s", out)
	}
	if got := sha("HEAD"); got != tagSHA {
		t.Fatalf("checkout at %s, want the release commit %s\n--- output ---\n%s", got, tagSHA, out)
	}
	if !strings.Contains(out, "moving the checkout BACK to v2026.11.01.1") {
		t.Errorf("the move back must be announced\n%s", out)
	}
	if !strings.Contains(out, "could not download") && !strings.Contains(out, "did not verify") {
		t.Errorf("expected the run to stop at the release download\n%s", out)
	}
	_ = clone
}

// TestUpdateRefusesToStrandLocalCommits — moving back is only safe for commits
// origin still has; a local-only commit refuses rather than disappearing.
func TestUpdateRefusesToStrandLocalCommits(t *testing.T) {
	clone, env, sha := realignFixture(t, "v2026.11.01.1")
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
	_, env, _ := realignFixture(t, "v2026.11.01.1")
	out, err := runScript(t, env, "update.sh", "--no-pull", "--tag", "v2026.11.01.1")
	if err == nil || !strings.Contains(out, "is not at v2026.11.01.1") {
		t.Fatalf("want a refusal, got err=%v\n%s", err, out)
	}
}
