# ADR-0010: microVM sandbox runtimes (Kata / libkrun) via a fail-closed `--runtime` selector

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers

## Context

The mandatory sandbox ([ADR-0002](0002-mandatory-rootless-podman-sandbox.md))
runs every agent tool call in a rootless-Podman container under the default OCI
runtime (`crun`/`runc`). Those containers **share the host kernel**: a kernel
CVE or a container-escape in the sandbox reaches the host directly. For
deployments handling sensitive data or untrusted prompts, that is the real
threat — you need to break the kernel, not just the container boundary.

Suna-tier deployments answer this with dedicated microVMs (Daytona/Platinum)
that boot a separate guest kernel per session, so an escape requires breaking
the **hypervisor** (KVM), a much higher bar. fleet can adopt the same posture
without replacing Podman or adding infrastructure: **Kata Containers** and
**libkrun** are OCI-compatible runtimes that run each container in a dedicated
KVM VM and plug into Podman through `--runtime=<value>` — an argument
`container.go` already emits.

What was missing was the operator-facing selector, the host-capability check,
and the guest-memory adjustment — plus a decision on how all that interacts with
the no-degrade-to-host invariant.

## Decision

A single knob — the client bundle's `manifest.yaml` `sandbox.runtime`, overridden
by the `FLEET_SANDBOX_RUNTIME` env var — selects the OCI runtime, emitted
verbatim as `podman run --runtime=<value>`. `""`/`runc`/`crun`/`runsc` keep the
existing shared-kernel (or gVisor) posture; `kata` and `libkrun` make every
sandbox container — one per turn, per scheduled run, or per conversation in
persistent-REPL mode — a dedicated KVM VM with its own guest kernel. The runtime
selection is
**trusted operator config**: the bundle manifest already pins the sandbox image
and Containerfile, so choosing the runtime is no greater an authority.

This **strengthens** ADR-0002 (an escape now needs a hypervisor CVE) and does
not weaken any other invariant — credentials still stay host-side, the seccomp
filter / dropped caps / read-only rootfs / network sealing / disk quota all
still apply; the microVM is an *additional* boundary.

Three supporting decisions make it safe:

1. **Fail-closed preflight.** fleet preflights **before the first container
   starts** and aborts boot on failure — it never silently falls back to a
   shared-kernel container, which would be a silent loss of the requested
   isolation. Two tiers:

   - **Any** non-empty runtime must be **resolvable by Podman**. The preflight
     asks Podman which binary it will exec (`podman --runtime=<r> info`, which
     resolves the name through `containers.conf` `[engine.runtimes]` and errors
     on an unregistered name) rather than guessing the binary from the name and
     looking it up on `PATH`. Guessing was wrong in two directions: a
     `containers.conf` remap pointed Podman at one binary while the preflight
     validated whichever same-named binary was first on `PATH`, and a runtime
     installed on `PATH` but never registered with Podman passed the preflight
     and then failed at every container creation. An empty runtime (Podman's
     own default) is the only no-op.
   - **`kata`/`krun`** additionally require read-write access to `/dev/kvm` (no
     usable KVM ⇒ no hypervisor isolation ⇒ refuse to start), and for `krun`
     the **resolved** binary must report `+LIBKRUN` (a plain `crun` renamed to
     `krun` would run as an ordinary container — the missing feature flag is a
     hard fail). `kata-runtime check` runs only as a **non-fatal warning**: run
     non-root it skips privileged checks and can exit non-zero for reasons that
     don't mean Kata is unusable, so gating on its exit code would break
     otherwise-healthy rootless-kata hosts.

2. **Name normalization.** `libkrun` is the product name; Podman's registered
   runtime is `krun`. fleet normalizes `libkrun → krun` (logged) so the manifest
   value the spec advertises actually works, while passing explicit paths
   verbatim.

3. **Kata memory overhead.** A Kata VM carries a ~512 MiB guest-kernel + VMM
   baseline. When `runtime=kata`, fleet adds `FLEET_SANDBOX_KATA_OVERHEAD_MB`
   (default 512) to the container's `--memory` so the operator-set limit still
   reflects usable guest RAM. The base limit is parsed with Podman's own
   conventions (a bare number is **bytes**; `k`/`m`/`g` are powers of 1024) and
   an **unparseable limit fails closed** rather than booting an undersized guest.
   The overhead is kata-only — libkrun's footprint is an order of magnitude
   smaller.

## Enforcement

- `internal/sandbox/oci_runtime.go` holds the normalization, fail-closed
  preflight, runtime→probe-binary mapping, and memory-overhead math;
  `internal/sandbox/oci_runtime_test.go` pins all of it with a **fake podman**,
  so the fail-closed paths are deterministic on any host: an unregistered
  runtime is rejected, the binary Podman resolves to (not the `PATH` guess) is
  the one probed, and — via `verifyKrunLibkrun`, split out precisely so the KVM
  gate cannot mask it on a CI host without `/dev/kvm` — a `crun` build lacking
  `+LIBKRUN` is rejected. `RuntimeBinary` remains a **best-effort** name→binary
  heuristic for the health probe; the ADR's guarantees rest on Podman's own
  resolution, not on it.
- The single production pool-construction path (`agent.buildSandboxPool`) calls
  `sandbox.PreflightRuntime` and returns its error, so a failed preflight aborts
  boot; `fleet validate-config` runs the same check as an operator preflight.
- `TestContainerKataRuntime` exercises a real kata sandbox end-to-end, skipping
  unless the host can actually run it (`/dev/kvm` + `kata-runtime`).

## Consequences

- Operators gain a hypervisor-isolation posture by setting one manifest field,
  at the cost of provisioning `/dev/kvm` + a microVM runtime and tolerating
  slower (~2 s) cold container boots — mitigated by raising the warm-pool depth.
- Naming **any** runtime now adds a boot-time dependency on a working Podman:
  a `sandbox.runtime: runsc` deployment aborts startup if `podman info` cannot
  resolve the name. That is a real widening of the fail-closed surface beyond
  kata/krun, taken deliberately — a runtime Podman cannot resolve would fail at
  every container creation anyway, so failing at boot with the reason is
  strictly more useful. Leaving the runtime unset keeps the previous behavior
  (no preflight, no boot-time Podman call).
- The preflight makes a misconfigured kata/krun host **fail loudly at boot**
  instead of running silently degraded. The trade is that a host whose KVM the
  fleet user can't open will refuse to start — which is the correct outcome for
  a deployment that explicitly asked for VM isolation.
- The default (empty runtime) is byte-for-byte unchanged, so existing
  deployments are unaffected.

## Alternatives considered

- **Replace Podman with a dedicated microVM manager (Daytona-style).** Rejected:
  it would fork the sandbox path and add infrastructure; Kata/libkrun reach the
  same isolation through the existing Podman API surface.
- **Silently fall back to `runc` when KVM is missing.** Rejected: it directly
  violates the no-degrade-to-host posture — an operator asking for kata and
  getting a shared-kernel container has a security story that is a fiction.
- **Fail-close on `kata-runtime check`'s exit code.** Rejected: non-root it is a
  reduced, sometimes-flaky signal; `/dev/kvm` is the authoritative gate, and
  breaking a healthy host buys no real security.
- **Pass `libkrun` to `--runtime` verbatim.** Rejected: Podman registers the
  runtime as `krun`, so the literal `libkrun` fails on a stock host — exactly the
  value the manifest spec advertises.
