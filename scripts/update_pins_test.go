// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinsFixture lays out a minimal repo (the updater, its libs, a workflow with a
// FOO pin) and an upstream serving release v1.1.0 of o/foo.
func pinsFixture(t *testing.T, published time.Time, tamper bool) (root string, run func() (string, error)) {
	return pinsFixtureTag(t, published, tamper, "v1.1.0")
}

func pinsFixtureTag(t *testing.T, published time.Time, tamper bool, tag string) (root string, run func() (string, error)) {
	t.Helper()
	src := repoRoot(t)
	root = t.TempDir()
	for _, rel := range []string{"scripts/update-pins.sh", "scripts/lib/release.sh", "internal/release/cosign.lock"} {
		b, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		if err := os.WriteFile(filepath.Join(root, rel), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(root, "scripts/lib/pins.tsv"),
		[]byte("FOO\tgithub\to/foo\tfoo_{v}_linux_amd64.tar.gz\tfoo_{v}_checksums.txt\t-\t-\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, ".github/workflows"), 0o755)
	oldSHA := strings.Repeat("a", 64)
	_ = os.WriteFile(filepath.Join(root, ".github/workflows/ci.yml"),
		[]byte("jobs:\n  x:\n    steps:\n      - env:\n          FOO_VERSION: '1.0.0'\n          FOO_SHA256: '"+oldSHA+"'\n"), 0o644)

	asset := []byte("foo 1.1.0 binary")
	sum := sha256.Sum256(asset)
	listed := hex.EncodeToString(sum[:])
	if tamper {
		listed = strings.Repeat("b", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/foo/releases":
			fmt.Fprintf(w, `[{"tag_name":%q,"draft":false,"prerelease":false,"published_at":%q},
			                {"tag_name":"v1.2.0-rc1","draft":false,"prerelease":true,"published_at":%q}]`,
				tag, published.UTC().Format(time.RFC3339), published.UTC().Format(time.RFC3339))
		case "/o/foo/releases/download/v1.1.0/foo_1.1.0_checksums.txt":
			fmt.Fprintf(w, "%s  foo_1.1.0_linux_amd64.tar.gz\n%s  foo_1.1.0_darwin_arm64.tar.gz\n", listed, strings.Repeat("c", 64))
		case "/o/foo/releases/download/v1.1.0/foo_1.1.0_linux_amd64.tar.gz":
			_, _ = w.Write(asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	run = func() (string, error) {
		cmd := exec.Command("bash", filepath.Join(root, "scripts/update-pins.sh"))
		cmd.Env = append(os.Environ(), "PINS_GITHUB_API="+srv.URL, "PINS_GITHUB_DOWNLOAD="+srv.URL, "GITHUB_OUTPUT=", "GITHUB_STEP_SUMMARY=")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return root, run
}

func TestUpdatePinsMovesAVerifiedPin(t *testing.T) {
	root, run := pinsFixture(t, time.Now().Add(-30*24*time.Hour), false)
	out, err := run()
	if err != nil {
		t.Fatalf("update-pins: %v\n%s", err, out)
	}
	wf, _ := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	sum := sha256.Sum256([]byte("foo 1.1.0 binary"))
	if !strings.Contains(string(wf), "FOO_VERSION: '1.1.0'") || !strings.Contains(string(wf), "FOO_SHA256: '"+hex.EncodeToString(sum[:])+"'") {
		t.Fatalf("pin not moved to the verified release:\n%s\n--- output ---\n%s", wf, out)
	}
}

func TestUpdatePinsRefusesAnAssetThatDoesNotMatch(t *testing.T) {
	root, run := pinsFixture(t, time.Now().Add(-30*24*time.Hour), true)
	out, err := run()
	if err == nil || !strings.Contains(out, "did not verify") {
		t.Fatalf("want a verification failure, got err=%v\n%s", err, out)
	}
	wf, _ := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if !strings.Contains(string(wf), "FOO_VERSION: '1.0.0'") {
		t.Fatalf("a pin moved to an unverified release:\n%s", wf)
	}
}

func TestUpdatePinsHonoursTheCooldown(t *testing.T) {
	root, run := pinsFixture(t, time.Now().Add(-24*time.Hour), false)
	out, err := run()
	if err != nil {
		t.Fatalf("update-pins: %v\n%s", err, out)
	}
	wf, _ := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if !strings.Contains(string(wf), "FOO_VERSION: '1.0.0'") {
		t.Fatalf("a release younger than the 7-day cooldown was adopted:\n%s", wf)
	}
}

// TestUpdatePinsRefusesHostileVersionStrings — an upstream tag is untrusted
// data: anything but a plain version is refused, and nothing in it can reach
// a program (the rewrite is an exact-match replacement fed through ENVIRON).
func TestUpdatePinsRefusesHostileVersionStrings(t *testing.T) {
	for _, tag := range []string{
		"v1.1.0|e",
		"v1.1.0'; touch /tmp/pwned; '",
		"v1.1.0/\\1/",
		"v1.1.0\nFOO_SHA256: 'x'",
		"$(id)",
	} {
		root, run := pinsFixtureTag(t, time.Now().Add(-30*24*time.Hour), false, tag)
		out, err := run()
		if err == nil || !strings.Contains(out, "not a plain version") {
			t.Errorf("tag %q: want a refusal, got err=%v\n%s", tag, err, out)
		}
		wf, _ := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
		if !strings.Contains(string(wf), "FOO_VERSION: '1.0.0'") || strings.Count(string(wf), "FOO_") != 2 {
			t.Errorf("tag %q changed the workflow:\n%s", tag, wf)
		}
	}
}

// TestUpdatePinsRewritesTheDocsThatRestateAPin — the prose copies listed in
// scripts/lib/pin-docs.tsv move with the pin, so the update PR needs no hand
// edits; only marked lines change, and only a whole version.
func TestUpdatePinsRewritesTheDocsThatRestateAPin(t *testing.T) {
	root, run := pinsFixture(t, time.Now().Add(-30*24*time.Hour), false)
	_ = os.WriteFile(filepath.Join(root, "scripts/lib/pin-docs.tsv"),
		[]byte("# comment\nFOO\tfoo\tv\tdocs/X.md\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	doc := "- **foo `v1.0.0`** pinned; go install example.com/foo@v1.0.0\n" +
		"- foo v1.0.01 and foo v1.0.0.1 are other versions\n" +
		"- bar v1.0.0 is an unmarked line\n"
	_ = os.WriteFile(filepath.Join(root, "docs/X.md"), []byte(doc), 0o644)
	if out, err := run(); err != nil {
		t.Fatalf("update-pins: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(root, "docs/X.md"))
	want := "- **foo `v1.1.0`** pinned; go install example.com/foo@v1.1.0\n" +
		"- foo v1.0.01 and foo v1.0.0.1 are other versions\n" +
		"- bar v1.0.0 is an unmarked line\n"
	if string(got) != want {
		t.Fatalf("docs rewrite:\n got %q\nwant %q", got, want)
	}
}
