package sandbox

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// storageProbeTimeout bounds the one-time boot probe for --storage-opt support.
// Generous because the first container off a freshly-pulled image can be slow to
// create; a probe that times out simply degrades to the ulimit fallback.
const storageProbeTimeout = 30 * time.Second

// ProbeStorageOptSupport reports whether `podman run --storage-opt size=...` is
// usable on this host. Podman validates the size quota against the storage driver
// + backing filesystem at container-create time (it works on overlay+xfs with
// pquota, btrfs, and zfs, but not overlay+ext4 or vfs), so we probe empirically:
// start a throwaway `--rm` container off the SAME sandbox image (already pulled)
// with a 1g cap running /usr/bin/true. A clean exit means quotas work; ANY
// failure — driver can't quota, image missing, timeout — returns false so the
// caller uses the always-safe `--ulimit fsize` fallback. Best-effort and
// side-effect-free (the container removes itself on exit).
func ProbeStorageOptSupport(ctx context.Context, podmanBin, image string) bool {
	if strings.TrimSpace(image) == "" {
		return false
	}
	if podmanBin == "" {
		podmanBin = "podman"
	}
	probeCtx, cancel := context.WithTimeout(ctx, storageProbeTimeout)
	defer cancel()

	args := make([]string, 0, 10)
	// Match the cgroup driver real container starts use (see podmanArgs).
	if runtime.GOOS == "linux" {
		args = append(args, "--cgroup-manager=cgroupfs")
	}
	args = append(args,
		"run", "--rm",
		"--name", generateContainerName(),
		"--storage-opt=size=1g",
		// No egress and a trivial, fast no-op command: we only care whether the
		// driver accepts the size quota, which podman checks before the command runs.
		"--network=none",
		"--entrypoint=/usr/bin/true",
		image,
	)
	cmd := exec.CommandContext(probeCtx, podmanBin, args...) //nolint:gosec // G204: fixed podman binary + operator-configured image; no user input.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("sandbox: --storage-opt size probe failed (%v): %s", err, strings.TrimSpace(stderr.String()))
		return false
	}
	return true
}
