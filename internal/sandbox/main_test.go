// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"log"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestMain warms the rootless-podman `--userns=keep-id` image cache ONCE before
// any timed test runs, eliminating an intermittent CI flake.
//
// Why: the isolation-invariant tests (workspace_same_path_test.go) each
// cold-start a container with `--userns=keep-id:uid=1000,gid=1000`, capped by
// NewContainer's 30s StartTimeout. With keep-id, podman must present the image
// rootfs with ownership shifted into the caller's subuid/subgid range. When the
// kernel/storage combo can't use idmapped mounts it falls back to chowning a
// PRIVATE COPY of every image layer into the rootless store — a one-time cost
// per (store, idmap) that, for our ~1.3GB Python/matplotlib image under noisy CI
// disk I/O, occasionally exceeds 30s. The chowned copy is then cached, so a
// re-run passes — the classic intermittent signature.
//
// Paying that cost here, with the IDENTICAL idmap the tests use, primes exactly
// the cache entry they need, so each subsequent `podman run` is fast and the 30s
// production default (a real-hang detector — we deliberately do NOT raise it)
// holds. This does not weaken any assertion: the tests still exercise the full
// keep-id + same-path-bind + userns path; we only move an environment-level
// one-time cost outside the timed window, exactly as the production warm Pool
// amortizes it at startup.
//
// Guarded to the same conditions the invariant tests run under (linux,
// non-root, podman present); a warm-up failure is logged and non-fatal so a
// genuinely broken environment still surfaces through the tests themselves.
func TestMain(m *testing.M) {
	warmKeepIDImageCache()
	os.Exit(m.Run())
}

func warmKeepIDImageCache() {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		return // the keep-id invariant tests skip here too — nothing to warm
	}
	if _, err := exec.LookPath("podman"); err != nil {
		return // tests will t.Skip("podman not available")
	}
	image := testImage()
	if image == "" {
		return
	}
	// Generous timeout: the chowned-layer copy is the cost we are explicitly
	// paying here (not in a 30s-capped test).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	log.Printf("sandbox test warm-up: priming keep-id idmap for %s …", image)
	// MUST match the invariant tests' idmap EXACTLY (keep-id:uid=1000,gid=1000),
	// or it primes the wrong cache entry and the flake persists. No mounts/name
	// needed — the cached layer is keyed by image + idmap, not by mounts.
	cmd := exec.CommandContext(ctx, "podman", "run", "--rm",
		"--userns=keep-id:uid=1000,gid=1000", image, "true")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("sandbox test warm-up: keep-id prime failed (continuing; tests will surface any real problem): %v\n%s", err, out)
		return
	}
	log.Printf("sandbox test warm-up: keep-id idmap primed in %s", time.Since(start).Round(time.Second))
}
