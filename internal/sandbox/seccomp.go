package sandbox

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"sync"
)

// defaultSeccompProfile is the curated OCI seccomp profile applied to every
// sandbox container via `--security-opt seccomp=<path>`. It is a default-deny
// allowlist (defaultAction SCMP_ACT_ERRNO) modeled on Docker's default profile
// but stricter: it withholds syscalls no bash / Python / file-IO / MCP-tool
// workload legitimately needs but that carry outsized kernel attack surface —
// ptrace, perf_event_open, keyctl, userfaultfd, the io_uring family, bpf,
// personality, and the kernel key-management calls (add_key/request_key).
//
// This is DEFENSE-IN-DEPTH layered on top of the existing --cap-drop=ALL +
// no-new-privileges + --read-only posture: capability drops and
// no-new-privileges do not filter individual syscalls, so an unprivileged
// process inside the container could still reach those calls without it. The
// profile only ADDS restriction; it never relaxes any existing isolation.
//
// clone3 is deliberately given SCMP_ACT_ERRNO with errnoRet=ENOSYS (38) rather
// than the default EPERM so glibc (>=2.34, which prefers clone3 for
// pthread_create / fork / posix_spawn) falls back to the allowlisted clone
// instead of hard-failing — without that, Python threading/multiprocessing and
// bash job control would break. See seccomp-default.json for the full list and
// per-syscall rationale, and sandbox_hardened_test.go for the regression net.
//
//go:embed seccomp-default.json
var defaultSeccompProfile []byte

// seccompProfileEnv overrides the bundled seccomp profile. Set it to:
//   - "none" (or "unconfined") to DISABLE seccomp filtering — debugging /
//     operator escape hatch only; logs a warning since it removes a security
//     layer.
//   - an absolute path to a custom OCI seccomp JSON file to use instead of the
//     bundled profile (the file is passed to podman verbatim).
//
// Unset (the default) uses the embedded profile written to a temp file.
const seccompProfileEnv = "FLEET_SANDBOX_SECCOMP_PROFILE"

// seccompUnconfinedWarnOnce limits the "seccomp disabled" warning to one line
// per process so an operator running with FLEET_SANDBOX_SECCOMP_PROFILE=none
// (every warm-pool fill spins up a container) doesn't flood the journal.
var seccompUnconfinedWarnOnce sync.Once

// resolveSeccompArg returns the value for `--security-opt seccomp=<value>` and a
// cleanup func the caller must defer. It honors FLEET_SANDBOX_SECCOMP_PROFILE:
//
//   - "none"/"unconfined" → "unconfined" (seccomp off) + no-op cleanup, with a
//     one-time warning that a security layer has been disabled.
//   - any other non-empty value → treated as a path to a custom profile, passed
//     through verbatim + no-op cleanup. (Operators owning the override own its
//     correctness.)
//   - empty (default) → writes the embedded profile to a temp file under
//     bridgeDir and returns that path + os.Remove as cleanup.
//
// The temp file lives in bridgeDir (same place as the bridge script), NOT
// os.TempDir(): production sets BridgeDir to escape systemd's PrivateTmp=
// namespace, which can otherwise hide /tmp from the rootless-podman OCI helpers
// that read the seccomp file at container-create time.
func resolveSeccompArg(bridgeDir string) (arg string, cleanup func(), err error) {
	noop := func() {}
	switch override := os.Getenv(seccompProfileEnv); override {
	case "":
		// Default: write the embedded profile to a temp file.
	case "none", "unconfined":
		seccompUnconfinedWarnOnce.Do(func() {
			// Log a fixed message (no operator-tainted value) — the only thing
			// that varies is which of the two literal keywords matched, which
			// the static text already covers.
			log.Printf("sandbox seccomp: DISABLED via %s=none/unconfined — the syscall filter defense-in-depth layer is OFF; --cap-drop=ALL + no-new-privileges still apply, but dangerous syscalls (ptrace, perf_event_open, bpf, io_uring, …) are reachable. Use for debugging only.", seccompProfileEnv)
		})
		return "unconfined", noop, nil
	default:
		// Custom profile path supplied by the operator — pass verbatim. The
		// path is operator-set config (an env var on the fleet process), not
		// agent/LLM/end-user input, so treating it as a trusted filesystem path
		// is correct; we Stat it only to fail loudly on a typo.
		if _, statErr := os.Stat(override); statErr != nil { //nolint:gosec // G703: override is operator-set config (FLEET_SANDBOX_SECCOMP_PROFILE), not untrusted input
			return "", noop, fmt.Errorf("%s=%q: %w", seccompProfileEnv, override, statErr)
		}
		return override, noop, nil
	}

	// Default path: materialize the embedded profile so podman can read it.
	f, err := os.CreateTemp(bridgeDir, "fleet-sandbox-seccomp-*.json")
	if err != nil {
		return "", noop, fmt.Errorf("temp seccomp file: %w", err)
	}
	path := f.Name()
	cleanupFile := func() { _ = os.Remove(path) }
	if _, err := f.Write(defaultSeccompProfile); err != nil {
		_ = f.Close()
		cleanupFile()
		return "", noop, fmt.Errorf("write seccomp profile: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanupFile()
		return "", noop, fmt.Errorf("close seccomp profile: %w", err)
	}
	// World-readable: the rootless-podman OCI runtime reads it as the mapped
	// user. The profile is non-secret embedded code that already ships in the
	// binary — same reasoning as the bridge script's 0o644 chmod.
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // non-secret embedded profile, must be readable by the rootless-podman runtime user
		cleanupFile()
		return "", noop, fmt.Errorf("chmod seccomp profile: %w", err)
	}
	return path, cleanupFile, nil
}
