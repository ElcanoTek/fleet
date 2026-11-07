// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/release"
)

// runReleaseLib sources scripts/lib/release.sh and runs one snippet.
func runReleaseLib(t *testing.T, env []string, snippet string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("bash", "-c", `set -u; . "$ROOT/scripts/lib/release.sh"; `+snippet)
	cmd.Env = append(append(os.Environ(), "ROOT="+root), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseLibRefusesAnUnpinnedCosign(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("no pinned cosign for this arch")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\necho pwned\n"))
	}))
	defer srv.Close()
	cache := t.TempDir()
	out, err := runReleaseLib(t, []string{"FLEET_COSIGN_URL=" + srv.URL, "CACHE=" + cache},
		`fleet_release_cosign "$ROOT" "$CACHE"`)
	if err == nil || !strings.Contains(out, "refusing to run it") {
		t.Fatalf("want a digest refusal, got err=%v out=%q", err, out)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("rejected cosign left files: %v", entries)
	}
}

func TestReleaseLibRefusesUnsignedReleases(t *testing.T) {
	cosign := os.Getenv("FLEET_TEST_COSIGN")
	if cosign == "" {
		t.Skip("set FLEET_TEST_COSIGN to the pinned cosign binary to run the shell verifier end to end")
	}
	if got, _ := release.FileSHA256(cosign); got != release.PinnedCosign().SHA256["linux/"+runtime.GOARCH] {
		t.Fatalf("FLEET_TEST_COSIGN is not the pinned cosign")
	}
	const tag = "v2026.11.07.3"
	arch := runtime.GOARCH
	files := map[string]string{
		"SHA256SUMS":                             "",
		"fleet-linux-" + arch:                    "binary",
		"release.json":                           `{"schema":1,"tag":"` + tag + `"}`,
		"SHA256SUMS.sigstore.json":               `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`,
		"fleet-linux-" + arch + ".sigstore.json": `{}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/"+tag+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(b))
	}))
	defer srv.Close()

	cache := t.TempDir()
	ver := release.PinnedCosign().Version
	raw, _ := os.ReadFile(cosign)
	if err := os.WriteFile(filepath.Join(cache, "cosign-"+ver+"-linux-"+arch), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"FLEET_RELEASE_BASE_URL=" + srv.URL, "CACHE=" + cache, "DIR=" + t.TempDir(), "TAG=" + tag,
		"HTTPS_PROXY=http://127.0.0.1:1", "FLEET_SIGSTORE_TRUSTED_ROOT=" + filepath.Join(repoRoot(t), "internal/release/testdata/sigstore-vector/trusted_root.json")}

	// A bundle that is not a signature by the release identity.
	out, err := runReleaseLib(t, env, `fleet_release_fetch "$ROOT" "$TAG" "$DIR" "$CACHE"`)
	if err == nil || !strings.Contains(out, "signature verification FAILED for SHA256SUMS") {
		t.Fatalf("garbage bundle: err=%v out=%q", err, out)
	}
	// No bundle at all.
	delete(files, "SHA256SUMS.sigstore.json")
	out, err = runReleaseLib(t, env, `fleet_release_fetch "$ROOT" "$TAG" "$DIR" "$CACHE"`)
	if err == nil || !strings.Contains(out, "could not download SHA256SUMS.sigstore.json") {
		t.Fatalf("missing bundle: err=%v out=%q", err, out)
	}
	// Not a tag.
	out, err = runReleaseLib(t, env, `fleet_release_fetch "$ROOT" main "$DIR" "$CACHE"`)
	if err == nil || !strings.Contains(out, "not a release tag") {
		t.Fatalf("bad tag: err=%v out=%q", err, out)
	}
}

func TestReleaseLibHealthURLMatchesGo(t *testing.T) {
	for _, addr := range []string{"", ":9090", "0.0.0.0:8080", "10.0.0.5:8080"} {
		out, err := runReleaseLib(t, []string{"ADDR=" + addr}, `fleet_release_health_url "$ADDR"`)
		if err != nil {
			t.Fatal(err)
		}
		if want := release.HealthURLFromAddr(addr); out != want {
			t.Errorf("%q: shell %q, Go %q", addr, out, want)
		}
	}
}
