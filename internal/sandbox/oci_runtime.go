// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// OCI-runtime selection (#217). The mandatory rootless-Podman sandbox can run
// under a hypervisor-isolated runtime — Kata Containers (a dedicated KVM VM
// with its own guest kernel) or libkrun (a lightweight microVM) — instead of
// the default shared-kernel container. The VM boundary has the granularity of
// the container it replaces (one per turn / scheduled run / persistent-REPL
// conversation), so every tool call in a turn shares it. This file holds
// the runtime-name normalization, the boot-time fail-closed preflight, the
// host-side probe binary mapping, and the Kata guest-memory overhead math. The
// `--runtime=<value>` flag itself is emitted unchanged in container.go; this is
// the policy/validation layer around it.
//
// SECURITY POSTURE: kata/krun STRENGTHEN the mandatory-sandbox invariant
// (ADR-0002) — an escape needs a hypervisor CVE, not a container-escape. The
// preflight FAILS CLOSED: a kata/krun runtime whose KVM or runtime binary is
// missing aborts boot rather than silently degrading to a shared-kernel
// container (the no-degrade-to-host invariant — ADR-0010). Any NAMED runtime,
// hypervisor-backed or not, must also be resolvable by Podman; only the empty
// default (Podman's own choice) skips the preflight entirely.

const (
	runtimeKata = "kata"
	runtimeKrun = "krun"
)

// DefaultKataOverheadMB is the memory added to a Kata container's --memory
// ceiling to cover the guest kernel + VMM (QEMU / Cloud-Hypervisor) baseline,
// so the operator-set limit still reflects USABLE guest memory rather than
// being eaten by the VM's own footprint. Overridable via
// FLEET_SANDBOX_KATA_OVERHEAD_MB.
//
// Kata-only: libkrun's in-process VMM overhead is an order of magnitude smaller
// (tens of MiB), so applying this fixed 512 MiB bump to krun would just
// over-allocate and starve other work for no benefit.
const DefaultKataOverheadMB = 512

const bytesPerMiB = 1 << 20

// NormalizeRuntime maps a friendly/manifest OCI-runtime name to the name Podman
// actually understands and reports whether it rewrote the value (so the caller
// can log the divergence). The returned value is what gets passed to
// `podman run --runtime=`.
//
//   - An empty value stays empty (Podman's configured default — crun/runc).
//   - A value containing a path separator is an explicit binary path and is
//     passed through VERBATIM — never rewritten — so an operator who points at
//     a specific runtime binary gets exactly that.
//   - A bare name is lower-cased (runtime names are lowercase by convention).
//   - "libkrun" — the project/product name — is rewritten to "krun", Podman's
//     registered runtime name for a crun build with +LIBKRUN. Passing the
//     literal "libkrun" to --runtime fails on a stock host, so we accept the
//     friendly name the manifest spec advertises and emit the real one. (An
//     operator who deliberately registered a literal `libkrun` alias in
//     containers.conf should use `krun` or an absolute path; the common case
//     wins and the rewrite is logged.)
func NormalizeRuntime(runtime string) (string, bool) {
	r := strings.TrimSpace(runtime)
	if r == "" {
		return "", false
	}
	// Explicit path: the operator means exactly that binary. Do not touch it.
	if strings.ContainsRune(r, os.PathSeparator) {
		return r, false
	}
	lower := strings.ToLower(r)
	if lower == "libkrun" {
		lower = runtimeKrun
	}
	return lower, lower != r
}

// runtimeKind classifies a runtime value — a bare name OR an explicit path —
// into the family that drives the fail-closed preflight and the Kata memory
// overhead: "kata", "krun", or "" (a shared-kernel/unknown runtime that needs
// neither). A PATH is classified by its basename, so `/opt/kata/bin/kata-runtime`
// and `/usr/local/bin/krun` get the SAME hypervisor treatment as the bare names
// — otherwise a path-form hypervisor runtime would silently skip the KVM gate
// and the memory bump, defeating the no-degrade invariant (ADR-0010).
func runtimeKind(runtime string) string {
	r, _ := NormalizeRuntime(runtime)
	if r == "" {
		return ""
	}
	base := r
	if strings.ContainsRune(r, os.PathSeparator) {
		base = strings.ToLower(filepath.Base(r))
	}
	switch {
	case base == runtimeKrun || base == "libkrun":
		return runtimeKrun
	case base == runtimeKata || strings.HasPrefix(base, "kata"):
		// "kata", "kata-runtime", "kata-qemu", … all map to the kata family.
		return runtimeKata
	default:
		return ""
	}
}

// ResolveRuntime applies the sandbox-runtime precedence and normalization in ONE
// place so every entrypoint (fleet boot, `fleet validate-config`, the cutlass
// task harness) resolves identically: an explicit env value
// (FLEET_SANDBOX_RUNTIME) wins, else the bundle manifest's sandbox.runtime, then
// NormalizeRuntime ("libkrun" → "krun", paths verbatim). Empty means Podman's
// configured default.
func ResolveRuntime(envRuntime, bundleRuntime string) string {
	raw := strings.TrimSpace(envRuntime)
	if raw == "" {
		raw = strings.TrimSpace(bundleRuntime)
	}
	normalized, _ := NormalizeRuntime(raw)
	return normalized
}

// RuntimeBinary returns a BEST-EFFORT guess at the executable Podman resolves
// `--runtime=<name>` to, for host-side liveness/readiness probes that only need
// a name to check. Podman maps the runtime NAME to a binary via containers.conf
// ([engine.runtimes]); "kata" resolves to the "kata-runtime" binary, not a
// "kata" command, so a bare LookPath("kata") would wrongly report it missing.
// An explicit path is returned verbatim (probe it directly). An empty runtime
// (Podman default) returns "" — callers probe "podman" instead.
//
// This is a HEURISTIC, not the resolution Podman performs: an operator whose
// containers.conf remaps the name to some other binary gets a different answer
// here than Podman will use. The fail-closed preflight therefore does NOT rely
// on it — PreflightRuntime asks Podman itself (resolveRuntimePath) and probes
// the binary Podman actually reports.
func RuntimeBinary(runtime string) string {
	r, _ := NormalizeRuntime(runtime)
	switch {
	case r == "":
		return ""
	case strings.ContainsRune(r, os.PathSeparator):
		return r // explicit path: probe it directly
	case r == runtimeKata:
		return "kata-runtime"
	default:
		// krun, runc, crun, runsc, …: the runtime name is the binary name.
		return r
	}
}

// runtimeResolveTimeout bounds the `podman info` call that resolves a runtime
// name to its binary. Generous enough for a cold podman on a loaded box, short
// enough that a wedged podman fails boot promptly instead of hanging it.
const runtimeResolveTimeout = 20 * time.Second

// resolveRuntimePath asks Podman which binary it will actually exec for
// `--runtime=<runtime>`, rather than guessing from the name.
//
// `podman --runtime=<r> info` is authoritative on both counts we care about: it
// resolves the name through containers.conf ([engine.runtimes]) and reports the
// resulting path, AND it fails outright when the name is not registered
// ("default OCI runtime %q not found"). Guessing instead — the old behavior —
// was wrong in two directions. A containers.conf remap pointed Podman at one
// binary while the preflight validated whichever same-named binary happened to
// be first on PATH; and a runtime installed on PATH but never registered with
// Podman passed the preflight, then failed at every container creation.
//
// Any failure here is fail-closed for the caller: a hypervisor runtime whose
// binary Podman cannot even name is one whose isolation we cannot verify.
func resolveRuntimePath(ctx context.Context, podmanBin, runtime string) (string, error) {
	if podmanBin == "" {
		podmanBin = "podman"
	}
	resolveCtx, cancel := context.WithTimeout(ctx, runtimeResolveTimeout)
	defer cancel()
	//nolint:gosec // podmanBin and runtime are operator-set config, not request input
	out, err := exec.CommandContext(resolveCtx, podmanBin, "--runtime="+runtime, "info", "--format", "{{.Host.OCIRuntime.Path}}").Output()
	if err != nil {
		return "", fmt.Errorf("podman could not resolve --runtime=%s (is it registered in containers.conf?): %w%s", runtime, err, stderrOf(err))
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("podman reported an empty binary path for --runtime=%s", runtime)
	}
	return path, nil
}

// PreflightRuntime verifies, BEFORE the first sandbox container starts, that
// the host can actually deliver what the configured OCI runtime promises.
// Called from the single production pool-construction path
// (agent.buildSandboxPool) and from `fleet validate-config`.
//
// Two tiers, both fail-closed:
//
//   - ANY non-empty runtime must be resolvable by Podman. This catches the
//     runtime that is installed but never registered in containers.conf, which
//     used to pass a PATH lookup and then fail at every single container
//     creation.
//   - kata/krun additionally need usable KVM and, for krun, a genuine +LIBKRUN
//     build (whether selected by bare name OR by path). A hypervisor runtime
//     that cannot deliver its isolation must abort boot, never degrade to a
//     shared-kernel container (ADR-0010).
//
// An empty runtime (Podman's own default) is a no-op — there is no name to
// resolve and no hardware to check.
//
// The binary probed is the one PODMAN resolves the runtime to, not a guess from
// the name (see resolveRuntimePath): validating a different binary than the one
// that will run every tool call is exactly the silent isolation loss ADR-0010
// exists to prevent.
func PreflightRuntime(ctx context.Context, podmanBin, runtime string) error {
	normalized, _ := NormalizeRuntime(runtime)
	if normalized == "" {
		return nil
	}
	kind := runtimeKind(runtime)
	// A resolution failure is a podman/containers.conf problem, so it is NOT
	// labelled "kata preflight:" / "krun preflight:" — those prefixes belong to
	// the hardware gates below and would misdirect an operator whose actual
	// problem is a broken rootless podman.
	bin, err := resolveRuntimePath(ctx, podmanBin, normalized)
	if err != nil {
		return fmt.Errorf("sandbox runtime preflight: %w", err)
	}
	// Surface a containers.conf remap rather than silently preflighting a
	// different binary than an operator reading the config would expect.
	if guess := RuntimeBinary(runtime); guess != "" && guess != bin && filepath.Base(bin) != guess {
		log.Printf("sandbox: podman resolves --runtime=%s to %s (the name suggests %q) — preflighting the resolved binary", normalized, bin, guess)
	}
	switch kind {
	case runtimeKata:
		return preflightKata(ctx, bin)
	case runtimeKrun:
		return preflightKrun(ctx, bin)
	default:
		// Shared-kernel runtime (runc/crun/runsc/…): resolvability is the whole
		// check — no hypervisor posture to verify.
		log.Printf("sandbox: runtime %s preflight OK — podman resolves it to %s", normalized, bin)
		return nil
	}
}

// kvmAccessible opens /dev/kvm read-write — the HARD gate for any
// hypervisor-isolated runtime. A bare os.Stat only proves the device node
// exists; an O_RDWR open proves the fleet process user can actually USE KVM (it
// needs membership in the `kvm` group). No usable KVM means no hypervisor
// isolation, so the preflight fails closed rather than booting a runtime that
// cannot deliver its security posture.
func kvmAccessible() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("/dev/kvm not accessible — KVM is required (is the fleet user in the kvm group?): %w", err)
	}
	_ = f.Close()
	return nil
}

// preflightKata fails closed unless the kata runtime binary (bin — the path
// PreflightRuntime got back from Podman) resolves and /dev/kvm is usable.
// `kata-runtime check` is a SOFT signal only: run non-root
// it skips the privileged/network checks and can exit non-zero for reasons that
// do not mean Kata is unusable, so its exit code is logged, NOT fail-closed —
// gating on it would break otherwise-healthy rootless-kata hosts. /dev/kvm is
// the real gate. --no-network-checks keeps a GitHub version check from delaying
// boot; the timeout bounds the KVM_CREATE_VM probe the check itself runs.
func preflightKata(ctx context.Context, bin string) error {
	if err := lookRuntimeBinary(bin); err != nil {
		return fmt.Errorf("kata preflight: %s not found — install Kata Containers: %w", bin, err)
	}
	if err := kvmAccessible(); err != nil {
		return fmt.Errorf("kata preflight: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(checkCtx, bin, "check", "--no-network-checks").CombinedOutput()
	if err != nil {
		log.Printf("sandbox: %s check reported issues (non-fatal — /dev/kvm is the hard gate): %v\n%s",
			bin, err, strings.TrimSpace(string(out)))
	} else {
		log.Printf("sandbox: kata preflight OK — %s present and /dev/kvm accessible", bin)
	}
	return nil
}

// preflightKrun fails closed unless the krun binary (bin) resolves, /dev/kvm is
// usable, AND that binary actually carries libkrun support (+LIBKRUN in its
// version banner). krun is crun built with +LIBKRUN; a plain crun symlinked or
// renamed to `krun` would run as an ordinary shared-kernel container — a SILENT
// loss of the VM isolation the operator asked for — so the missing feature flag
// is a hard fail, not a pass.
func preflightKrun(ctx context.Context, bin string) error {
	// Binary first (mirrors preflightKata) so a missing krun reports "not found"
	// rather than the less-actionable "/dev/kvm not accessible".
	if err := lookRuntimeBinary(bin); err != nil {
		return fmt.Errorf("krun preflight: %s not found — install crun built with libkrun support: %w", bin, err)
	}
	if err := kvmAccessible(); err != nil {
		return fmt.Errorf("krun preflight: %w", err)
	}
	if err := verifyKrunLibkrun(ctx, bin); err != nil {
		return fmt.Errorf("krun preflight: %w", err)
	}
	log.Printf("sandbox: krun (libkrun) preflight OK — %s has +LIBKRUN and /dev/kvm accessible", bin)
	return nil
}

// verifyKrunLibkrun is the "is this really libkrun?" half of the krun preflight,
// split out so it is testable on a host without /dev/kvm (the KVM gate runs
// first and would otherwise mask this check everywhere CI runs).
//
// krun is crun built with +LIBKRUN. A plain crun symlinked or renamed to `krun`
// would run as an ordinary shared-kernel container — a SILENT loss of the VM
// isolation the operator asked for — so a missing feature flag is a hard fail,
// not a pass. Case-insensitive to tolerate banner-format drift across crun
// releases.
func verifyKrunLibkrun(ctx context.Context, bin string) error {
	verCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(verCtx, bin, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%q --version failed: %w", bin, err)
	}
	if !strings.Contains(strings.ToUpper(string(out)), "+LIBKRUN") {
		return fmt.Errorf("%q lacks +LIBKRUN — this is plain crun without libkrun support and would NOT provide microVM isolation", bin)
	}
	return nil
}

// lookRuntimeBinary confirms the preflight's probe binary resolves. Podman
// reports an absolute path, so in practice the Stat branch is the one that
// runs; the PATH-lookup branch remains for a direct caller passing a bare name.
// Either way a missing/unexecutable binary fails closed.
func lookRuntimeBinary(bin string) error {
	if filepath.IsAbs(bin) {
		info, err := os.Stat(bin)
		if err != nil {
			return err
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s is not an executable file", bin)
		}
		return nil
	}
	_, err := exec.LookPath(bin)
	return err
}

// kataOverheadMB resolves the Kata guest-memory overhead in MiB:
// FLEET_SANDBOX_KATA_OVERHEAD_MB when set to a positive integer, else
// DefaultKataOverheadMB.
//
// The knob is a scopeExternal row in the env-knob registry (#1273), so a
// malformed or non-positive value REFUSES THE BOOT (config.Load validates the
// whole registry) instead of being quietly ignored as it was before. The
// local fallback below therefore only ever fires for a value that appeared
// after boot — it stays because this path cannot fail a container create for a
// tuning knob that only ever ADDS memory (unlike the memory LIMIT itself,
// which fails closed when unparseable).
func kataOverheadMB() int {
	if v := strings.TrimSpace(os.Getenv("FLEET_SANDBOX_KATA_OVERHEAD_MB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		//nolint:gosec // G706: v is operator-set env config (FLEET_SANDBOX_KATA_OVERHEAD_MB), quoted with %q — not request input.
		log.Printf("sandbox: ignoring invalid FLEET_SANDBOX_KATA_OVERHEAD_MB=%q — using default %dMiB", v, DefaultKataOverheadMB)
	}
	return DefaultKataOverheadMB
}

// ParseMemoryLimitBytes parses a Podman/Docker --memory string into bytes. It
// is the exported face of parseMemoryToBytes for callers outside this package
// that need to reason about the configured limit — e.g. the boot-time check
// that PersistentMaxSessions x MemoryLimit does not oversubscribe host RAM.
// Exported as a wrapper rather than by renaming so the Kata sizing path keeps
// its unexported, package-local call.
func ParseMemoryLimitBytes(s string) (uint64, error) {
	n, err := parseMemoryToBytes(s)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative memory value %q", s)
	}
	return uint64(n), nil
}

// parseMemoryToBytes parses a Podman/Docker --memory string into bytes. Two
// traps this gets right:
//
//   - A BARE number is BYTES, matching Podman's own convention
//     (`--memory 536870912` == 512 MiB). Treating it as MiB would over-allocate
//     by ~1,000,000×.
//   - The b/k/m/g suffixes are powers of 1024 (bytes/KiB/MiB/GiB), not 1000.
//
// Decimals are rejected — Podman's --memory takes an integer, so "1.5g" is not
// a value Podman accepts either. Anything unparseable returns an error so the
// caller can FAIL CLOSED rather than ship a guest that may be too small to boot.
func parseMemoryToBytes(s string) (int64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	lower := strings.ToLower(raw)
	mult := int64(1) // bare number = bytes
	num := lower
	switch last := lower[len(lower)-1]; {
	case last == 'b':
		mult, num = 1, lower[:len(lower)-1]
	case last == 'k':
		mult, num = 1024, lower[:len(lower)-1]
	case last == 'm':
		mult, num = bytesPerMiB, lower[:len(lower)-1]
	case last == 'g':
		mult, num = 1<<30, lower[:len(lower)-1]
	case last >= '0' && last <= '9':
		// bare number — bytes, mult stays 1
	default:
		return 0, fmt.Errorf("invalid memory suffix in %q (want b|k|m|g or a bare byte count)", raw)
	}
	val, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory number in %q: %w", raw, err)
	}
	if val <= 0 {
		return 0, fmt.Errorf("memory value %q must be positive", raw)
	}
	if val > math.MaxInt64/mult {
		return 0, fmt.Errorf("memory value %q overflows int64 bytes", raw)
	}
	return val * mult, nil
}

// addKataMemoryOverhead returns memoryLimit with the Kata guest overhead added:
// it ceils the base limit up to whole MiB (never under-provisioning below the
// request), adds overheadMB, and reformats as "<N>m". It FAILS CLOSED when the
// base limit can't be parsed — without a known limit we cannot guarantee the
// guest has enough RAM to boot.
func addKataMemoryOverhead(memoryLimit string, overheadMB int) (string, error) {
	b, err := parseMemoryToBytes(memoryLimit)
	if err != nil {
		return "", fmt.Errorf("apply kata memory overhead: %w", err)
	}
	if overheadMB < 0 {
		return "", fmt.Errorf("apply kata memory overhead: negative overhead %dMiB", overheadMB)
	}
	// Ceil to whole MiB WITHOUT overflowing — `(b + MiB - 1)` would wrap for a
	// base near math.MaxInt64. Divide first, then round up on any remainder.
	baseMiB := b / bytesPerMiB
	if b%bytesPerMiB != 0 {
		baseMiB++
	}
	overhead := int64(overheadMB)
	if baseMiB > math.MaxInt64-overhead {
		return "", fmt.Errorf("apply kata memory overhead: %q + %dMiB overflows int64", memoryLimit, overheadMB)
	}
	return fmt.Sprintf("%dm", baseMiB+overhead), nil
}

// applyKataMemoryOverhead bumps cfg.MemoryLimit by the Kata guest overhead when
// the configured runtime is kata, leaving every other runtime untouched. Called
// from NewContainer AFTER applyContainerDefaults (so MemoryLimit is resolved to
// a concrete value) and AFTER any per-task ResourceOverride (so the overhead
// stacks on the FINAL limit). It operates on the fresh per-container cfg value,
// so the bump applies exactly once and never accumulates across containers.
// Returns an error — propagated out of NewContainer — when the limit is
// unparseable, so a misconfiguration fails closed instead of silently shipping
// an undersized guest.
func applyKataMemoryOverhead(cfg *ContainerConfig) error {
	if runtimeKind(cfg.Runtime) != runtimeKata {
		return nil
	}
	bumped, err := addKataMemoryOverhead(cfg.MemoryLimit, kataOverheadMB())
	if err != nil {
		return err
	}
	cfg.MemoryLimit = bumped
	return nil
}
