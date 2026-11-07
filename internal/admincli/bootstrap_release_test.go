// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapInstallsTheNewestPublishedRelease — with no --tag, bootstrap
// asks for the newest PUBLISHED release (not whatever tag the clone has), moves
// the checkout to it and re-runs from there. The fixture serves no assets, so
// the re-run stops at the download; what is asserted is the choice of release
// and the move.
func TestBootstrapInstallsTheNewestPublishedRelease(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		t.Skip("a real bootstrap run would install packages on this dnf host")
	}
	root := repoRootFromTest(t)
	home := t.TempDir()
	gitEnv := append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// A repo carrying the real scripts/ (and the cosign lock they read):
	// A = published release, B = tagged but not published yet, C = branch tip.
	base := t.TempDir()
	work := filepath.Join(base, "work")
	copyTree(t, filepath.Join(root, "scripts"), filepath.Join(work, "scripts"))
	copyTree(t, filepath.Join(root, "internal", "release"), filepath.Join(work, "internal", "release"))
	git(work, "init", "-q", "-b", "main")
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "A")
	git(work, "tag", "-a", "v2026.11.01.1", "-m", "published")
	_ = os.WriteFile(filepath.Join(work, "b"), []byte("b\n"), 0o644)
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "B")
	git(work, "tag", "-a", "v2026.11.02.1", "-m", "assets still building")
	_ = os.WriteFile(filepath.Join(work, "c"), []byte("c\n"), 0o644)
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "C")
	origin := filepath.Join(base, "origin.git")
	git(base, "clone", "-q", "--bare", work, origin)
	clone := filepath.Join(base, "clone")
	git(base, "clone", "-q", origin, clone)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2026.11.01.1"}`))
	}))
	defer api.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	defer dead.Close()
	cmd := exec.Command("bash", filepath.Join(clone, "scripts", "bootstrap.sh"), "--postgres=external")
	cmd.Dir = clone
	cmd.Env = append(append([]string{}, gitEnv...), "TERM=dumb", "FLEET_RELEASE_LATEST_URL="+api.URL, "FLEET_RELEASE_BASE_URL="+dead.URL,
		"FLEET_COSIGN_URL="+dead.URL, "FLEET_RELEASE_CACHE_DIR="+t.TempDir(), "FLEET_STATE_DIR="+t.TempDir(),
		"FLEET_ENV_FILE="+filepath.Join(t.TempDir(), "fleet.env"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the fixture serves no assets, so bootstrap must stop at the download\n%s", out)
	}
	if got, want := git(clone, "rev-parse", "HEAD"), git(clone, "rev-parse", "v2026.11.01.1^{commit}"); got != want {
		t.Fatalf("checkout at %s, want the PUBLISHED release %s\n%s", got, want, out)
	}
	for _, want := range []string{"checkout moved to v2026.11.01.1", "re-running bootstrap from it", "could not be downloaded and verified"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}
