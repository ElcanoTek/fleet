// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scripts/install-pinned-tool.sh is how CI installs its release-asset tools
// (gitleaks, actionlint, kubeconform, golangci-lint, grype, cosign) so a GitHub
// release-asset outage does not fail the job: a verified cache, then the
// checksum-pinned download, then `go install` of the same version through the
// Go module proxy. These tests pin the order and, above all, that a checksum
// mismatch is fatal and never falls through to the fallback.

type pinnedToolEnv struct {
	dir, cache, dest, log, tarball, sha string
	curlMode                            string // "ok", "fail", "bad"
}

func newPinnedToolEnv(t *testing.T) *pinnedToolEnv {
	t.Helper()
	dir := t.TempDir()
	e := &pinnedToolEnv{dir: dir, cache: filepath.Join(dir, "cache", "tool"), dest: filepath.Join(dir, "dest"), log: filepath.Join(dir, "calls.log")}
	if err := os.MkdirAll(e.dest, 0o755); err != nil {
		t.Fatal(err)
	}

	// The release asset: a tarball holding sub/tool.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho release-build\n")
	if err := tw.WriteHeader(&tar.Header{Name: "sub/tool", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	e.tarball = filepath.Join(dir, "upstream.tar.gz")
	if err := os.WriteFile(e.tarball, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	e.sha = hex.EncodeToString(sum[:])

	// Stub curl and go on PATH; each logs that it ran.
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	curl := `#!/usr/bin/env bash
echo curl >> "$STUB_LOG"
out=""
while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac; done
case "$STUB_CURL" in
  ok)  cp "$STUB_TARBALL" "$out" ;;
  bad) echo tampered > "$out" ;;
  *)   echo "curl: (22) The requested URL returned error: 504" >&2; exit 22 ;;
esac
`
	goStub := `#!/usr/bin/env bash
echo "go $* GOPROXY=$GOPROXY GOSUMDB=$GOSUMDB GONOSUMDB=$GONOSUMDB GOFLAGS=$GOFLAGS" >> "$STUB_LOG"
printf '#!/bin/sh\necho source-build\n' > "$GOBIN/tool"
chmod 0755 "$GOBIN/tool"
`
	for name, src := range map[string]string{"curl": curl, "go": goStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(src), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *pinnedToolEnv) run(t *testing.T, sha string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{filepath.Join(repoRoot(t), "scripts", "install-pinned-tool.sh"),
		"--url", "https://github.invalid/o/r/releases/download/v1.2.3/tool.tar.gz",
		"--sha256", sha, "--member", "sub/tool", "--cache-dir", e.cache, "--dest", e.dest}, extra...)
	args = append(args, "tool")
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(e.dir, "bin")+":"+os.Getenv("PATH"),
		"STUB_LOG="+e.log, "STUB_CURL="+e.curlMode, "STUB_TARBALL="+e.tarball,
		"GONOSUMDB=example.com/*", "GOFLAGS=-mod=vendor")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *pinnedToolEnv) calls() string { b, _ := os.ReadFile(e.log); return string(b) }

func (e *pinnedToolEnv) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dest, "tool"))
	if err != nil {
		t.Fatalf("nothing installed: %v", err)
	}
	return string(b)
}

func TestInstallPinnedToolDownloadsVerifiesAndCaches(t *testing.T) {
	e := newPinnedToolEnv(t)
	e.curlMode = "ok"
	if out, err := e.run(t, e.sha); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(e.installed(t), "release-build") {
		t.Fatal("did not install the release binary")
	}
	if _, err := os.Stat(filepath.Join(e.cache, "tool.tar.gz")); err != nil {
		t.Fatalf("verified asset not cached: %v", err)
	}

	// Second run: the cache answers, curl never runs, even with GitHub down.
	_ = os.Remove(e.log)
	_ = os.Remove(filepath.Join(e.dest, "tool"))
	e.curlMode = "fail"
	if out, err := e.run(t, e.sha, "--go-install", "example.com/tool@v1.2.3"); err != nil {
		t.Fatalf("cached install failed: %v\n%s", err, out)
	}
	if c := e.calls(); c != "" {
		t.Fatalf("a cache hit touched the network or the fallback: %q", c)
	}
	if !strings.Contains(e.installed(t), "release-build") {
		t.Fatal("cache hit did not install the cached release binary")
	}
}

func TestInstallPinnedToolRefetchesATamperedCache(t *testing.T) {
	e := newPinnedToolEnv(t)
	if err := os.MkdirAll(e.cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cache, "tool.tar.gz"), []byte("poisoned"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.curlMode = "ok"
	if out, err := e.run(t, e.sha); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if !strings.Contains(e.calls(), "curl") || !strings.Contains(e.installed(t), "release-build") {
		t.Fatal("a cache entry that does not match the pin was used instead of re-downloaded")
	}
}

func TestInstallPinnedToolChecksumMismatchIsFatalAndNeverFallsBack(t *testing.T) {
	e := newPinnedToolEnv(t)
	e.curlMode = "bad"
	out, err := e.run(t, e.sha, "--go-install", "example.com/tool@v1.2.3")
	if err == nil {
		t.Fatalf("a tampered download was accepted:\n%s", out)
	}
	if strings.Contains(e.calls(), "go ") {
		t.Fatal("a checksum mismatch fell through to go install — it must fail, a mismatch is not an outage")
	}
	if _, err := os.Stat(filepath.Join(e.dest, "tool")); err == nil {
		t.Fatal("a tampered download was installed")
	}
	if _, err := os.Stat(filepath.Join(e.cache, "tool.tar.gz")); err == nil {
		t.Fatal("a tampered download was cached")
	}
}

func TestInstallPinnedToolFallsBackToGoInstallOnOutage(t *testing.T) {
	e := newPinnedToolEnv(t)
	e.curlMode = "fail"
	out, err := e.run(t, e.sha, "--go-install", "example.com/tool@v1.2.3")
	if err != nil {
		t.Fatalf("outage with a fallback failed: %v\n%s", err, out)
	}
	if !strings.Contains(e.installed(t), "source-build") {
		t.Fatal("fallback binary not installed")
	}
	c := e.calls()
	if !strings.Contains(c, "go install example.com/tool@v1.2.3") {
		t.Fatalf("go install not run at the pinned version: %q", c)
	}
	for _, want := range []string{"GOPROXY=https://proxy.golang.org ", "GOSUMDB=sum.golang.org ", "GONOSUMDB= ", "GOFLAGS="} {
		if !strings.Contains(c, want) {
			t.Errorf("go install ran without %s (the job environment must not loosen the module proxy / checksum DB): %q", want, c)
		}
	}
	if _, err := os.Stat(e.cache); err == nil {
		t.Fatal("the fallback left a cache directory behind; actions/cache would save it empty")
	}
}

func TestInstallPinnedToolOutageWithoutFallbackFails(t *testing.T) {
	e := newPinnedToolEnv(t)
	e.curlMode = "fail"
	if out, err := e.run(t, e.sha); err == nil {
		t.Fatalf("an outage with no fallback reported success:\n%s", out)
	}
	if out, err := e.run(t, e.sha, "--go-install", "example.com/tool@latest"); err == nil || !strings.Contains(out, "pinned version") {
		t.Fatalf("an unpinned --go-install was accepted: %v\n%s", err, out)
	}
}

// The installer's download is a curl like every workflow curl, so it carries
// the same bounded retry TestWorkflowNetworkFetchesRetry pins there.
func TestInstallPinnedToolCurlRetries(t *testing.T) {
	// The command, not "(curl exit N)" in a message.
	cmdRe := regexp.MustCompile(`^(?:if\s+)?curl\s`)
	n := 0
	for _, line := range workflowLogicalLines(readFile(t, repoRoot(t), "scripts/install-pinned-tool.sh")) {
		if !cmdRe.MatchString(line) {
			continue
		}
		n++
		for _, want := range []string{"-fsSL", "--retry ", "--retry-all-errors", "--connect-timeout "} {
			if !strings.Contains(line, want) {
				t.Errorf("install-pinned-tool.sh: curl without %s: %q", strings.TrimSpace(want), line)
			}
		}
	}
	if n != 1 {
		t.Fatalf("found %d curl invocations in install-pinned-tool.sh, want 1", n)
	}
}
