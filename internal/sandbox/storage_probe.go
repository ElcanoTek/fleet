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
// create; a probe that times out simply omits the writable-layer quota (the
// per-file ulimit cap applies regardless).
const storageProbeTimeout = 30 * time.Second

// ProbeStorageOptSupport reports whether `podman run --storage-opt size=...` is
// usable on this host. Podman validates the size quota against the storage driver
// + backing filesystem at container-create time (it works on overlay+xfs with
// pquota, btrfs, and zfs, but not overlay+ext4 or vfs), so we probe empirically:
// start a throwaway `--rm` container off the SAME sandbox image (already pulled)
// with a 1g cap running /usr/bin/true. A clean exit means quotas work.
//
// A failure to EXEC that command also means quotas work, and is treated as
// success. Podman validates the storage option inside `configure storage` at
// container-create time, before the runtime is invoked at all. Verified in both
// directions: on an ext4 host `--storage-opt=size=1g` with a nonexistent
// entrypoint reports the *quota* error rather than the exec error, and on
// overlay+XFS with prjquota (where the quota IS accepted) the same missing
// entrypoint surfaces the crun exec error. So reaching the exec stage proves the
// quota was accepted.
//
// This matters because the sandbox image is a client-bundle artifact free to
// change its base, and `/usr/bin/true` is a rootfs assumption: a busybox-based
// bundle has `/bin/true`. Without this carve-out such a bundle would silently
// lose the writable-layer quota on every host, quota-capable or not.
//
// Two probe failures on a quota-capable host still report NO support: a
// `/usr/bin/true` that exists but is not executable, or whose interpreter is
// broken. Both produce different messages, so both fall through to false. That
// is the conservative direction and is left as-is.
//
// A cleaner mechanism exists and is worth a follow-up: `podman create` validates
// the quota and leaves no container, so a create+rm probe would need no rootfs
// assumption and no error-string matching at all (podman/crun wording is not
// API). Deliberately not folded in here — it changes the probe's shape and
// deserves its own verification pass.
//
// Any OTHER failure — driver can't quota, image missing, timeout — returns false,
// which simply omits the writable-layer quota; the per-file `--ulimit fsize` cap
// is applied regardless and is what bounds the workspace bind mount. Best-effort
// and side-effect-free (the container removes itself on exit).
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
		detail := strings.TrimSpace(stderr.String())
		if probeCommandUnavailable(detail) {
			// The quota was accepted; only the probe's own command was missing
			// from this bundle's rootfs. Report support, and say so rather than
			// leaving an operator to wonder why a quota-capable host logged a
			// probe failure.
			log.Printf("sandbox: --storage-opt size probe could not exec /usr/bin/true in %s (%s) — the quota itself was ACCEPTED (podman validates it before the exec), so treating the driver as quota-capable", image, detail)
			return true
		}
		log.Printf("sandbox: --storage-opt size probe failed (%v): %s", err, detail)
		return false
	}
	return true
}

// probeCommandUnavailable reports whether a probe failure was the OCI runtime
// being unable to invoke the probe command, as opposed to podman rejecting the
// storage quota. The two are distinguishable by message because they happen at
// different stages: podman validates --storage-opt at container-create time and
// only then hands off to the runtime to exec.
//
// The markers are deliberately NARROW. A false positive here is worse than the
// bug this carve-out fixes: reporting quota-capable on a driver that is not means
// `--storage-opt=size` gets passed to every real container, and every container
// start then fails. So only phrases specific to "the runtime could not invoke the
// command" qualify. A bare "no such file or directory" does not — it appears in
// unrelated podman failures (missing socket, bad graphroot) that must keep
// returning false.
func probeCommandUnavailable(stderr string) bool {
	low := strings.ToLower(stderr)
	for _, marker := range []string{
		"attempted to invoke a command that was not found",
		"executable file not found",
		"executable file `", // crun: executable file `/usr/bin/true` not found
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}
