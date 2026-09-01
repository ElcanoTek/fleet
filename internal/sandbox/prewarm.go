// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// Boot-time keep-id image pre-warm (#1358).
//
// Every fleet sandbox runs under keepIDUserns, and the FIRST keep-id run of a
// given image makes podman build an id-remapped copy of every image layer into
// the rootless store — a one-time cost per (image, idmap) that measured 88s
// wall for the multi-GB sandbox image on WSL2. Before this pre-warm existed,
// that cost was paid inside NewContainer's StartTimeout (default 30s): the
// context expiry SIGKILLed podman mid-copy ("signal: killed", empty stderr),
// every retry restarted the copy from scratch, and the box looked bricked
// after every sandbox image update until an operator happened to run a keep-id
// `podman run` by hand.
//
// PrewarmKeepIDImage pays the copy up front, at boot, under a generous budget
// and with an honest log line — before the warm pool spawns its first
// container. A marker file records the image ID the store was last warmed
// for, so an unchanged image costs one `podman image inspect` (~100ms) per
// boot, not a throwaway container run. Best-effort by design: a pre-warm
// failure logs and continues, because the per-start timeout (now tunable via
// FLEET_SANDBOX_START_TIMEOUT_SECONDS) plus the named timeout error in
// container.go remain the backstop, and a genuinely broken podman surfaces
// through the pool exactly as before.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// prewarmTimeout bounds the one throwaway keep-id run. Generous on purpose:
// the copy is the cost being paid here, and it scales with image size and
// disk speed (88s measured on WSL2 for a multi-GB image — minutes are normal
// on slow I/O, and a bounded wait beats a wedged deployment).
const prewarmTimeout = 15 * time.Minute

// PrewarmKeepIDImage primes podman's id-remapped layer copy for image under
// the exact idmap every sandbox start uses (keepIDUserns), when the marker at
// markerPath says the local image changed since the last warm (or no warm ever
// happened). Called from the podman-backend boot path before the warm pool
// fills; the kubernetes backend never calls it (pods have their own start
// ceiling and no keep-id copy). Best-effort: errors are logged, never fatal.
func PrewarmKeepIDImage(ctx context.Context, podmanBinary, image, markerPath string) {
	if podmanBinary == "" {
		podmanBinary = "podman"
	}
	imageID, err := podmanImageID(ctx, podmanBinary, image)
	if err == nil && imageID != "" && imageID == readPrewarmMarker(markerPath) {
		return // this exact image was already warmed on this store
	}

	log.Printf("sandbox: preparing the keep-id id-remapped copy of image %s — the first start after a sandbox image update can take minutes on slow disks (#1358)", image)
	runCtx, cancel := context.WithTimeout(ctx, prewarmTimeout)
	defer cancel()
	start := time.Now()
	// MUST use keepIDUserns verbatim: the cached copy is keyed by
	// (image, idmap), so a different idmap primes the wrong entry. No mounts,
	// name, or runtime flag needed — the copy is runtime-independent.
	cmd := exec.CommandContext(runCtx, podmanBinary, "run", "--rm", keepIDUserns, image, "true") //nolint:gosec // podman binary + image are operator-configured, not user input
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("sandbox: keep-id pre-warm failed after %s (continuing — sandbox starts will pay the id-remap cost under FLEET_SANDBOX_START_TIMEOUT_SECONDS instead): %v\n%s",
			time.Since(start).Round(time.Second), err, out)
		return
	}
	log.Printf("sandbox: keep-id id-remapped image copy ready in %s", time.Since(start).Round(time.Second))

	// The run may have pulled the image, so resolve the ID now if the inspect
	// above failed. A marker write failure only costs a redundant warm next
	// boot (~2s once the copy is cached) — log and move on.
	if imageID == "" {
		if imageID, err = podmanImageID(ctx, podmanBinary, image); err != nil || imageID == "" {
			return
		}
	}
	if err := writePrewarmMarker(markerPath, imageID); err != nil {
		log.Printf("sandbox: keep-id pre-warm marker write failed (the next boot re-warms): %v", err)
	}
}

// podmanImageID resolves the LOCAL image ID for image ("" with an error when
// the image is not in the local store yet — the pre-warm run then pulls it).
func podmanImageID(ctx context.Context, podmanBinary, image string) (string, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(inspectCtx, podmanBinary, "image", "inspect", "--format", "{{.Id}}", image).Output() //nolint:gosec // operator-configured inputs
	if err != nil {
		return "", fmt.Errorf("podman image inspect %s: %w", image, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func readPrewarmMarker(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // marker path is derived from the operator-configured data dir
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writePrewarmMarker(path, imageID string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // data dir, same posture as its siblings
		return err
	}
	return os.WriteFile(path, []byte(imageID+"\n"), 0o644) //nolint:gosec // an image ID is not secret
}
