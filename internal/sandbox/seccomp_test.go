package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// ociSeccompProfile is the subset of the OCI Runtime Spec linux.seccomp shape
// we need to assert on the bundled profile.
type ociSeccompProfile struct {
	DefaultAction string `json:"defaultAction"`
	Architectures []string
	Syscalls      []struct {
		Names    []string `json:"names"`
		Action   string   `json:"action"`
		ErrnoRet *int     `json:"errnoRet"`
		Args     []struct {
			Index    int    `json:"index"`
			Value    uint64 `json:"value"`
			ValueTwo uint64 `json:"valueTwo"`
			Op       string `json:"op"`
		} `json:"args"`
	} `json:"syscalls"`
}

func parseDefaultProfile(t *testing.T) ociSeccompProfile {
	t.Helper()
	var p ociSeccompProfile
	if err := json.Unmarshal(defaultSeccompProfile, &p); err != nil {
		t.Fatalf("embedded seccomp-default.json is not valid JSON: %v", err)
	}
	return p
}

// allowedNames returns the union of all SCMP_ACT_ALLOW syscall names in the
// bundled profile.
func allowedNames(p ociSeccompProfile) map[string]struct{} {
	out := map[string]struct{}{}
	for _, blk := range p.Syscalls {
		if blk.Action == "SCMP_ACT_ALLOW" {
			for _, n := range blk.Names {
				out[n] = struct{}{}
			}
		}
	}
	return out
}

// TestEmbeddedSeccompProfileIsDefaultDeny pins the core security shape: the
// bundled profile must be embedded, parse cleanly, and DENY by default (anything
// not on the allowlist returns an errno). A regression that flips defaultAction
// to SCMP_ACT_ALLOW would silently turn the filter into a no-op.
func TestEmbeddedSeccompProfileIsDefaultDeny(t *testing.T) {
	if len(defaultSeccompProfile) == 0 {
		t.Fatal("defaultSeccompProfile is empty — //go:embed seccomp-default.json regressed")
	}
	p := parseDefaultProfile(t)
	if p.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatalf("defaultAction = %q, want SCMP_ACT_ERRNO (default-deny) — a non-deny default makes the filter a no-op", p.DefaultAction)
	}
	if len(allowedNames(p)) == 0 {
		t.Fatal("profile has no SCMP_ACT_ALLOW syscalls — every syscall would be denied and nothing would run")
	}
}

// TestSeccompProfileAllowsWorkloadSyscalls pins that the syscalls bash / Python
// (threading, multiprocessing, numpy/scipy) / pip / file-IO legitimately need
// stay on the allowlist. Removing any of these would break legitimate sandbox
// tool calls — the live e2e runs real bash/run_python against this profile.
func TestSeccompProfileAllowsWorkloadSyscalls(t *testing.T) {
	p := parseDefaultProfile(t)
	allowed := allowedNames(p)
	// futex/mmap/mprotect/mremap: Python threading + numpy/scipy.
	// clone: multiprocessing + bash job control (clone3 falls back to it).
	// socket: pip install + outbound HTTP in non-lockdown turns.
	// prctl/arch_prctl/set_thread_area/rseq: libc + thread-local storage setup.
	// ioctl: tty / terminal handling in bash, many file ops.
	// execve/openat/read/write: the absolute basics.
	required := []string{
		"futex", "mmap", "mprotect", "mremap", "clone", "socket", "prctl",
		"arch_prctl", "rseq", "ioctl", "execve", "openat", "read", "write",
		"clock_gettime", "getrandom", "epoll_pwait", "rt_sigaction",
	}
	for _, name := range required {
		if _, ok := allowed[name]; !ok {
			t.Errorf("required syscall %q is NOT allowlisted — this breaks legitimate bash/python/pip workloads", name)
		}
	}
}

// TestSeccompProfileBlocksDangerousSyscalls pins that the high-attack-surface
// syscalls the profile is meant to withhold are NOT on the allowlist (and so
// fall through to the default SCMP_ACT_ERRNO deny). This is the defense-in-depth
// contract from #219 — a regression that allowlisted any of these would reopen
// the kernel attack surface --cap-drop=ALL does not cover.
func TestSeccompProfileBlocksDangerousSyscalls(t *testing.T) {
	p := parseDefaultProfile(t)
	allowed := allowedNames(p)
	dangerous := []string{
		"ptrace", "perf_event_open", "keyctl", "add_key", "request_key",
		"userfaultfd", "bpf", "personality",
		"io_uring_setup", "io_uring_enter", "io_uring_register",
		// vmsplice: podman's OWN default profile denies it
		// (SCMP_ACT_ERRNO/EPERM in /usr/share/containers/seccomp.json), and
		// ours used to allow it — making this profile WEAKER than shipping no
		// custom profile at all in that one dimension, which is the opposite of
		// what #219 set out to do. It grants a process the ability to splice
		// pages of its own address space into a pipe, a primitive that has
		// featured in kernel-memory-disclosure and container-escape exploits.
		// Verified nothing in the sandbox needs it: bash pipes, cp, gzip-less
		// coreutils, 20 MB pipe/copy workloads, shutil.copyfile, os.sendfile
		// zero-copy, pandas and numpy all work with it denied.
		"vmsplice",
	}
	for _, name := range dangerous {
		if _, ok := allowed[name]; ok {
			t.Errorf("dangerous syscall %q IS allowlisted — it must be denied (defense-in-depth, #219)", name)
		}
	}
}

// TestSeccompProfileClone3FallsBackToENOSYS pins the single most fragile detail
// of the profile: clone3 must return ENOSYS (38), not the default EPERM, so
// glibc >= 2.34 (which prefers clone3 for pthread_create/fork/posix_spawn)
// gracefully falls back to the allowlisted clone. With EPERM, Python threading
// and subprocess creation hard-fail and the live e2e breaks.
func TestSeccompProfileClone3FallsBackToENOSYS(t *testing.T) {
	p := parseDefaultProfile(t)
	allowed := allowedNames(p)
	if _, ok := allowed["clone3"]; ok {
		t.Fatal("clone3 must NOT be SCMP_ACT_ALLOW — it must return ENOSYS so libc falls back to clone")
	}
	var found bool
	for _, blk := range p.Syscalls {
		if !slices.Contains(blk.Names, "clone3") {
			continue
		}
		found = true
		if blk.Action != "SCMP_ACT_ERRNO" {
			t.Errorf("clone3 action = %q, want SCMP_ACT_ERRNO", blk.Action)
		}
		const enosys = 38
		if blk.ErrnoRet == nil || *blk.ErrnoRet != enosys {
			t.Errorf("clone3 errnoRet = %v, want %d (ENOSYS) so glibc falls back to clone", blk.ErrnoRet, enosys)
		}
	}
	if !found {
		t.Fatal("clone3 has no explicit rule — it would inherit the default EPERM and break libc's clone3->clone fallback")
	}
}

// TestResolveSeccompArgDefault pins that, with no env override, resolveSeccompArg
// materializes the embedded profile into a readable temp file under the supplied
// bridgeDir (NOT os.TempDir — production sets BridgeDir to escape PrivateTmp=),
// and that the cleanup removes it.
func TestResolveSeccompArgDefault(t *testing.T) {
	t.Setenv(seccompProfileEnv, "")
	bridgeDir := t.TempDir()

	arg, cleanup, err := resolveSeccompArg(bridgeDir)
	if err != nil {
		t.Fatalf("resolveSeccompArg: %v", err)
	}
	if !strings.HasPrefix(arg, bridgeDir+string(os.PathSeparator)) {
		t.Fatalf("seccomp arg %q is not under bridgeDir %q (PrivateTmp=/rootless-podman would hide a /tmp-resident profile from the OCI helpers)", arg, bridgeDir)
	}
	data, err := os.ReadFile(arg)
	if err != nil {
		t.Fatalf("read materialized profile: %v", err)
	}
	if string(data) != string(defaultSeccompProfile) {
		t.Fatal("materialized profile does not match the embedded default")
	}
	info, err := os.Stat(arg)
	if err != nil {
		t.Fatalf("stat profile: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("profile mode = %v, want 0644 (rootless-podman runtime user must read it)", info.Mode().Perm())
	}

	cleanup()
	if _, err := os.Stat(arg); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove the temp profile %q (err=%v)", arg, err)
	}
}

// TestResolveSeccompArgNone pins the operator debugging escape hatch: the "none"
// and "unconfined" overrides disable seccomp by returning the literal
// "unconfined" keyword podman understands.
func TestResolveSeccompArgNone(t *testing.T) {
	for _, v := range []string{"none", "unconfined"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(seccompProfileEnv, v)
			arg, cleanup, err := resolveSeccompArg(t.TempDir())
			if err != nil {
				t.Fatalf("resolveSeccompArg: %v", err)
			}
			defer cleanup()
			if arg != "unconfined" {
				t.Fatalf("seccomp arg = %q, want %q (disables seccomp for debugging)", arg, "unconfined")
			}
		})
	}
}

// TestResolveSeccompArgCustomPath pins that a custom profile path is passed
// through verbatim, and that a nonexistent path is a hard error (so a typo in
// the operator override fails loudly at container start rather than silently
// falling back to a different policy).
func TestResolveSeccompArgCustomPath(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-seccomp.json")
	if err := os.WriteFile(custom, []byte(`{"defaultAction":"SCMP_ACT_ALLOW"}`), 0o644); err != nil {
		t.Fatalf("write custom profile: %v", err)
	}
	t.Setenv(seccompProfileEnv, custom)
	arg, cleanup, err := resolveSeccompArg(t.TempDir())
	if err != nil {
		t.Fatalf("resolveSeccompArg: %v", err)
	}
	defer cleanup()
	if arg != custom {
		t.Fatalf("seccomp arg = %q, want custom path %q passed verbatim", arg, custom)
	}

	t.Setenv(seccompProfileEnv, filepath.Join(t.TempDir(), "does-not-exist.json"))
	if _, cleanup, err := resolveSeccompArg(t.TempDir()); err == nil {
		cleanup()
		t.Fatal("expected error for nonexistent custom profile path, got nil — a typo'd override must fail loudly, not silently change policy")
	}
}

// afVsock is the AF_VSOCK address family. Under the Kata/libkrun microVM
// runtimes (ADR-0010) it is the guest<->host channel, so the sandboxed payload
// must not be able to open one.
const afVsock = 40

// TestSeccompProfileDeniesAFVSock pins that `socket` is NOT unconditionally
// allowed and that AF_VSOCK specifically falls through to the default-deny.
//
// The shape matters as much as the outcome. Podman's own default expresses this
// with an explicit ERRNO rule for AF_VSOCK plus several broad `SCMP_CMP_NE`
// ALLOW rules — and copying that verbatim does NOT work here, because one of
// those broad allows (`arg0 != AF_NETLINK`) also matches AF_VSOCK and absorbs
// the narrow deny. Measured, not assumed: that arrangement reproduced podman's
// NETLINK_AUDIT denial but left AF_VSOCK open.
//
// So this profile uses ONE non-overlapping rule instead — allow every family
// except AF_VSOCK — and lets defaultAction (SCMP_ACT_ERRNO) do the denying.
// Verified against the real image: AF_VSOCK returns EPERM while AF_UNIX,
// AF_INET, AF_INET6, AF_INET datagram and AF_NETLINK all still open, and DNS,
// HTTPS and `pip install` all work.
func TestSeccompProfileDeniesAFVSock(t *testing.T) {
	p := parseDefaultProfile(t)

	var unconditional, argScoped int
	sawVsockExclusion := false
	for _, blk := range p.Syscalls {
		if blk.Action != "SCMP_ACT_ALLOW" || !slices.Contains(blk.Names, "socket") {
			continue
		}
		if len(blk.Args) == 0 {
			unconditional++
			continue
		}
		argScoped++
		for _, a := range blk.Args {
			if a.Index == 0 && a.Value == afVsock && a.Op == "SCMP_CMP_NE" {
				sawVsockExclusion = true
			}
			// A broad NE on a DIFFERENT family also matches AF_VSOCK and would
			// absorb the deny — the exact trap described above.
			if a.Index == 0 && a.Op == "SCMP_CMP_NE" && a.Value != afVsock {
				t.Errorf("socket ALLOW rule excludes only family %d, which still matches AF_VSOCK (%d) and re-opens it", a.Value, afVsock)
			}
		}
	}
	if unconditional > 0 {
		t.Errorf("socket has %d unconditional ALLOW rule(s) — AF_VSOCK is then reachable regardless of any deny rule", unconditional)
	}
	if argScoped == 0 {
		t.Fatal("socket has no argument-scoped ALLOW rule — sockets would be denied outright, breaking all networking")
	}
	if !sawVsockExclusion {
		t.Error("no socket ALLOW rule excludes AF_VSOCK; nothing keeps the guest<->host channel closed")
	}
}

// TestSeccompProfileDeniesAtRuntime is the runtime counterpart to the static
// assertions above: it proves the bundled profile actually reaches a live
// container and denies what it claims to.
//
// The existing ptrace canary lives in sandbox_hardened_test.go, which is opt-in
// behind FLEET_SANDBOX_HARDENED_TEST and therefore runs in NO CI lane. This one
// is only podman-gated, so it runs in a normal `make test` on a podman host —
// and it is named in the e2e-live invariant list in .github/workflows/ci.yml,
// the one always-on lane with real rootless podman, which also treats a SKIP as
// a failure. Being selected there is what makes it catch a profile-plumbing
// regression (a mis-resolved path, a dropped --security-opt); the `go` job masks
// podman, so it would self-skip there.
func TestSeccompProfileDeniesAtRuntime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("seccomp is linux-only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	profile, cleanup, err := resolveSeccompArg(t.TempDir())
	if err != nil {
		t.Fatalf("resolveSeccompArg: %v", err)
	}
	defer cleanup()
	if profile == "unconfined" {
		t.Skip("seccomp disabled via " + seccompProfileEnv)
	}

	// AF_VSOCK must be refused; AF_INET must still open. Both in one container so
	// a filter that denied everything would fail the second assertion too.
	const probe = `import socket, sys
try:
    s = socket.socket(40, socket.SOCK_STREAM); s.close(); print("VSOCK_OPEN")
except OSError:
    print("VSOCK_DENIED")
try:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM); s.close(); print("INET_OPEN")
except OSError as e:
    print("INET_DENIED", e)
`
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "run", "--rm",
		"--security-opt", "seccomp="+profile, "--cap-drop=ALL", "--network=none",
		testImage(), "python3", "-c", probe).CombinedOutput()
	if err != nil {
		t.Fatalf("probe container failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "VSOCK_DENIED") {
		t.Errorf("AF_VSOCK was NOT denied inside a live container — the guest<->host channel is open. Output:\n%s", got)
	}
	if !strings.Contains(got, "INET_OPEN") {
		t.Errorf("AF_INET did not open — the profile is over-restrictive, not just strict. Output:\n%s", got)
	}
}
