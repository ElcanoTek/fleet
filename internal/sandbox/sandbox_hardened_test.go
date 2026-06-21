package sandbox_test

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// sandboxEnv reads the canonical FLEET_SANDBOX_* variable, falling back
// to the legacy CHAT_SANDBOX_* name for back-compat.
func sandboxEnv(name string) string {
	if v := os.Getenv("FLEET_SANDBOX_" + name); v != "" {
		return v
	}
	return os.Getenv("CHAT_SANDBOX_" + name)
}

// TestSandboxUnderSystemdHardening is the regression net for production
// outage class: the sandbox unit-tests above run as root with no systemd
// wrapper, so a podman flag that ONLY breaks under chat-server.service's
// hardening profile (see deploy/chat-server.service) sails past CI and
// hits users on first deploy.
//
// The bug that motivated this test: rootless podman's default systemd
// cgroup driver places container scopes under
// user-987.slice/user@987.service/user.slice/, which is in a different
// cgroup subtree than chat-server.service's own cgroup
// (system.slice/chat-server.service). Migrating processes across that
// LCA is denied for the chat user, so every `podman exec` failed with
// `write to .../cgroup.procs: Permission denied`. Fix lives in
// container.go:podmanArgs. This test pins the contract so removing
// that flag breaks `go test`.
//
// Skipped unless:
//   - GOOS=linux
//   - systemd-run + podman + go on PATH
//   - $CHAT_SANDBOX_HARDENED_TEST=1 (opt-in: the test mutates host state
//     by allocating a chat user + linger if missing, and pulls a real
//     OCI image, neither of which we want to trigger from a casual `go
//     test ./...` run on a developer laptop).
//   - $CHAT_SANDBOX_TEST_IMAGE set, or the default published image
//     reachable.
func TestSandboxUnderSystemdHardening(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hardening test runs on linux only")
	}
	if sandboxEnv("HARDENED_TEST") != "1" {
		t.Skip("set FLEET_SANDBOX_HARDENED_TEST=1 (or legacy CHAT_SANDBOX_HARDENED_TEST=1) to run (mutates host: creates 'chat' user, pulls images)")
	}
	for _, tool := range []string{"systemd-run", "podman", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	if os.Geteuid() != 0 {
		t.Skip("must run as root (drives systemd-run with --uid=chat)")
	}

	// /opt/chat/bin is whitelisted in the unit's ReadWritePaths and the
	// chat user can read it; t.TempDir() lives under /tmp/go-build* which
	// ProtectSystem=strict masks from the unit's mount namespace.
	if err := os.MkdirAll("/opt/chat/bin", 0o755); err != nil {
		t.Fatalf("mkdir /opt/chat/bin: %v", err)
	}
	probe := "/opt/chat/bin/sandbox-probe-test"
	build := exec.CommandContext(t.Context(), "go", "build", "-o", probe, "../../cmd/sandbox-probe")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sandbox-probe: %v\n%s", err, out)
	}
	defer os.Remove(probe)
	if err := exec.CommandContext(t.Context(), "chown", "chat:chat", probe).Run(); err != nil {
		t.Fatalf("chown probe: %v", err)
	}
	if err := os.Chmod(probe, 0o755); err != nil {
		t.Fatalf("chmod probe: %v", err)
	}

	image := sandboxEnv("TEST_IMAGE")
	if image == "" {
		image = "ghcr.io/elcanotek/sandbox:latest"
	}

	// Mirror the hardening profile from deploy/chat-server.service. If a
	// directive is added to / removed from the unit, mirror it here so
	// the regression net keeps tracking reality.
	args := []string{
		"--pipe", "--wait", "--quiet",
		"--uid=chat", "--gid=chat",
		"--slice=system.slice",
		"--setenv=HOME=/opt/chat",
		"--setenv=XDG_RUNTIME_DIR=/run/user/987",
		"--setenv=SANDBOX_IMAGE=" + image,
		"--property=Delegate=yes",
		"--property=NoNewPrivileges=false",
		"--property=ProtectSystem=strict",
		// PrivateDevices= is intentionally omitted — see the unit file
		// for why (rootless podman's pasta needs /dev/net/tun).
		"--property=ProtectKernelTunables=true",
		"--property=ProtectKernelModules=true",
		"--property=LockPersonality=true",
		"--property=ReadWritePaths=/opt/chat/data /opt/chat/workspace /opt/chat/.local /opt/chat/.config /tmp",
		"--working-directory=/opt/chat",
		probe,
	}
	cmd := exec.CommandContext(t.Context(), "systemd-run", args...)
	cmd.Dir = "/tmp" // systemd-run inherits cwd; keep it out of $HOME for the chat user
	out, err := cmd.CombinedOutput()
	t.Logf("sandbox-probe under hardened scope:\n%s", out)
	if err != nil {
		t.Fatalf("systemd-run: %v", err)
	}
	s := string(out)

	// Anti-regression assertions. The exact failure mode this test pins:
	// crun cgroup.procs writes failing with EACCES, which would surface
	// as "Permission denied: OCI permission denied" or "broken pipe" in
	// the bridge stderr.
	if strings.Contains(s, "OCI permission denied") {
		t.Fatalf("crun cgroup.procs write failed — fix in container.go:podmanArgs has regressed:\n%s", out)
	}
	if !strings.Contains(s, "BASH exit=0") {
		t.Fatalf("expected BASH exit=0, got:\n%s", out)
	}
	if !strings.Contains(s, "PYTHON status=success") {
		t.Fatalf("expected PYTHON status=success, got:\n%s", out)
	}
}

// TestSandboxUnderRestrictSUIDSGIDFails is the canary that pins WHY we
// removed RestrictSUIDSGID=true from deploy/chat-server.service. It
// reruns the hardened scope above with that single directive added back
// and asserts the same failure operators saw in production:
//
//	Error: creating container storage: creating an ID-mapped copy of
//	layer "...": error during chown: storage-chown-by-maps:
//	chmod usr/bin/chage: operation not permitted
//
// Why have this test at all: the directive looks innocuous (it just
// blocks setting suid/sgid bits) and a future hardening pass can be
// tempted to re-add it without realizing it kills rootless podman's
// `--userns=keep-id:uid=N,gid=N` ID-mapped layer copy on any image
// shipping setuid/setgid binaries (shadow-utils, util-linux — i.e.
// every Fedora/RHEL/Debian base). Both connector entry points
// (Pool.Take in non-lockdown, Pool.TakeContainer in lockdown) funnel
// through that podman run; both fail with the same chown error.
// Without this regression test the directive could be re-added in a
// future hardening pass and bash/run_python would stop working
// silently for every user on the next `chat update`.
//
// This test EXPECTS the failure: it passes when chown fails with the
// known signature, and fails (== regression) only if either (a) the
// directive stops blocking the chmod (kernel/podman behavior change —
// worth investigating before celebrating) or (b) `podman run`
// surprisingly succeeds (in which case re-evaluate whether the
// directive is safe to re-add).
//
// Same opt-in env as TestSandboxUnderSystemdHardening.
func TestSandboxUnderRestrictSUIDSGIDFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hardening test runs on linux only")
	}
	if sandboxEnv("HARDENED_TEST") != "1" {
		t.Skip("set FLEET_SANDBOX_HARDENED_TEST=1 (or legacy CHAT_SANDBOX_HARDENED_TEST=1) to run (mutates host: creates 'chat' user, pulls images)")
	}
	for _, tool := range []string{"systemd-run", "podman"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	if os.Geteuid() != 0 {
		t.Skip("must run as root (drives systemd-run with --uid=chat)")
	}

	image := sandboxEnv("TEST_IMAGE")
	if image == "" {
		image = "ghcr.io/elcanotek/sandbox:latest"
	}

	// Force a fresh ID-mapped layer copy by purging any cached one. The
	// chown happens once per (source layer, uid mapping) pair and is
	// cached in the overlay-layers store; without this prune, a previous
	// successful run (e.g. the test above, or the operator's manual
	// `podman pull` warming) would let `podman run` skip the chown
	// entirely and the assertion would silently pass for the wrong
	// reason.
	prune := exec.CommandContext(t.Context(), "sudo", "-u", "chat", "-H", "podman", "rmi", "-f", image)
	prune.Dir = "/opt/chat"
	_ = prune.Run() // best-effort; if the image isn't there we'll re-pull below

	// Re-pull so the image exists; the ID-mapped copy still has to be
	// constructed at first `podman run` time, which is what we want to
	// trip.
	pull := exec.CommandContext(t.Context(), "sudo", "-u", "chat", "-H", "podman", "pull", image)
	pull.Dir = "/opt/chat"
	if out, err := pull.CombinedOutput(); err != nil {
		t.Fatalf("podman pull %s: %v\n%s", image, err, out)
	}

	// Same mirror as above + the one directive we're pinning.
	args := []string{
		"--pipe", "--wait", "--quiet",
		"--uid=chat", "--gid=chat",
		"--slice=system.slice",
		"--setenv=HOME=/opt/chat",
		"--setenv=XDG_RUNTIME_DIR=/run/user/987",
		"--property=Delegate=yes",
		"--property=NoNewPrivileges=false",
		"--property=ProtectSystem=strict",
		// PrivateDevices= is omitted to mirror the prod unit. The
		// RestrictSUIDSGID directive below is the one we're pinning here.
		"--property=ProtectKernelTunables=true",
		"--property=ProtectKernelModules=true",
		"--property=RestrictSUIDSGID=true",
		"--property=LockPersonality=true",
		"--property=ReadWritePaths=/opt/chat/data /opt/chat/workspace /opt/chat/.local /opt/chat/.config /tmp",
		"--working-directory=/opt/chat",
		"/usr/bin/podman", "--cgroup-manager=cgroupfs", "run", "--rm",
		"--userns=keep-id:uid=1000,gid=1000",
		"--network=none",
		image, "/bin/echo", "OK",
	}
	cmd := exec.CommandContext(t.Context(), "systemd-run", args...)
	cmd.Dir = "/tmp"
	out, err := cmd.CombinedOutput()
	t.Logf("podman run under RestrictSUIDSGID=true:\n%s", out)
	if err == nil {
		t.Fatalf("expected `podman run` to fail under RestrictSUIDSGID=true (the directive that motivated removing it from the unit), got success: %s", out)
	}
	s := string(out)
	// The exact failure signature we're pinning. Match on the high-bits
	// substrings — kernel/podman wording can drift around them, but if
	// the chown-by-maps + chmod EPERM combo isn't there, something else
	// failed and we should know.
	if !strings.Contains(s, "storage-chown-by-maps") {
		t.Fatalf("RestrictSUIDSGID=true failure no longer mentions storage-chown-by-maps — re-investigate before drawing conclusions:\n%s", out)
	}
	if !strings.Contains(s, "operation not permitted") {
		t.Fatalf("RestrictSUIDSGID=true failure no longer mentions EPERM — re-investigate before drawing conclusions:\n%s", out)
	}
}
