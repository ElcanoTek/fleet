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
// process inside the container could still reach those calls without it.
//
// Relative to PODMAN'S DEFAULT profile (which is what a container gets with no
// --security-opt seccomp at all) this profile is stricter in every dimension we
// have measured EXCEPT ONE: it still allows socket(2) with
// AF_NETLINK+NETLINK_AUDIT, which podman denies. See the comparison below.
//
// That exception is stated up front on purpose. This file used to claim it "only
// ADDS restriction; it never relaxes any existing isolation", and that was false
// twice over — vmsplice (now fixed) and the socket cases (AF_VSOCK now fixed,
// NETLINK_AUDIT still open). Treat "only adds restriction" as a goal this file is
// held to by tests, not as a property that follows from shipping a custom
// profile.
//
// clone3 is deliberately given SCMP_ACT_ERRNO with errnoRet=ENOSYS (38) rather
// than the default EPERM so glibc (>=2.34, which prefers clone3 for
// pthread_create / fork / posix_spawn) falls back to the allowlisted clone
// instead of hard-failing — without that, Python threading/multiprocessing and
// bash job control would break. seccomp-default.json holds the full allowlist
// (a bare names array — JSON has no comments, so the rationale lives here and in
// seccomp_test.go); seccomp_test.go pins the shape statically and
// sandbox_hardened_test.go proves the filter actually reaches a live container.
//
// HOW THIS PROFILE COMPARES TO PODMAN'S OWN DEFAULT
// (/usr/share/containers/seccomp.json). Ours is a strict allowlist. Podman's
// default UNCONDITIONALLY allows eight syscalls that ours denies — verified at
// runtime under --cap-drop=ALL, i.e. they reach the kernel there and return
// EPERM/ENOSYS here: ptrace, process_vm_readv, keyctl, mount, umount2,
// pivot_root, unshare, setns.
//
// Three more are worth stating precisely, because reading podman's file
// carelessly gets them backwards (this comment did, once):
//
//   - userfaultfd: podman DENIES it too — it sits in the same deny block as
//     vmsplice. Ours denies it. No difference.
//   - bpf and perf_event_open: podman's rules are capability-CONDITIONAL
//     (includes/excludes on CAP_SYS_ADMIN / CAP_BPF / CAP_PERFMON). Under
//     --cap-drop=ALL podman's effective action is DENY, same as ours.
//   - personality: podman allows only a fixed set of argument values and denies
//     ADDR_NO_RANDOMIZE. Not a blanket allow.
//
// The lesson those three encode: podman's profile is a TEMPLATE whose
// includes/excludes podman resolves against the container's capability set
// before handing the filter to the OCI runtime. The file on disk is not the
// effective filter, and comparing against it without accounting for that
// produces confident, wrong conclusions.
//
// ONE dimension where ours is still weaker: socket(2) with
// AF_NETLINK+NETLINK_AUDIT, which podman denies (EINVAL) and we allow — writing
// the kernel audit log from inside the sandbox. Closing it is harder than
// AF_VSOCK was: podman expresses it as an ERRNO rule on (AF_NETLINK,
// NETLINK_AUDIT) alongside a broad allow, and reproducing that ordering here
// did not deny it.
//
// AF_VSOCK — the guest<->host channel under the Kata/libkrun runtimes — is now
// denied, but be precise about how far that goes:
//
//   - It stops the ORDINARY calling convention. socket(AF_VSOCK, …) returns
//     EPERM. See TestSeccompProfileDeniesAFVSock for the rule shape and why one
//     non-overlapping rule beats copying podman's five.
//   - It does NOT stop a hostile payload. seccomp compares the full 64-bit
//     register while the kernel truncates `domain` to int, so
//     socket(0x100000028, …) — AF_VSOCK with any high bit set — still succeeds.
//     Verified from inside the sandbox with six lines of ctypes, i.e. by exactly
//     the actor ADR-0002 threat-models.
//   - PODMAN'S OWN DEFAULT HAS THE IDENTICAL BYPASS, measured the same way. So
//     this is parity with the platform default, not a fleet-specific weakness,
//     and the rule is still worth having: it closes the accidental and the
//     library-mediated cases.
//   - Two obvious hardenings were tried and REJECTED on measurement, not taste.
//     A single rule ANDing two comparisons on arg0 (NE 40 plus LT 2^32) fails
//     OPEN — plain AF_VSOCK became reachable again and the container still
//     started. A SCMP_CMP_MASKED_EQ deny is absorbed by the broad NE allow,
//     whichever order they appear in: the same trap documented above.
//
// Also unguarded, and noted rather than fixed: socketcall(2) is unconditionally
// allowed while `architectures` still lists SCMP_ARCH_X86 / X32 / ARM. On i386,
// socket() goes through socketcall, which passes its arguments via a memory
// array seccomp cannot inspect, so the AF_VSOCK rule does not cover that path.
// Podman allows socketcall unconditionally too (and its archMap omits i386,
// where ours does not). Practically gated by the image shipping no 32-bit
// loader.
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

// seccompProfileTempPattern is the os.CreateTemp pattern for the materialized
// default profile written into bridgeDir. Also a filepath.Match glob:
// PruneOrphanedBridgeFiles keys on it to sweep files a crash orphaned (the
// deferred cleanup in start() removes it on the graceful path only) — keep the
// two uses in lockstep.
const seccompProfileTempPattern = "fleet-sandbox-seccomp-*.json"

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
	f, err := os.CreateTemp(bridgeDir, seccompProfileTempPattern)
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
