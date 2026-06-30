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
existing shared-kernel (or gVisor) posture; `kata` and `libkrun` give each tool
call a dedicated KVM VM with its own guest kernel. The runtime selection is
**trusted operator config**: the bundle manifest already pins the sandbox image
and Containerfile, so choosing the runtime is no greater an authority.

This **strengthens** ADR-0002 (an escape now needs a hypervisor CVE) and does
not weaken any other invariant — credentials still stay host-side, the seccomp
filter / dropped caps / read-only rootfs / network sealing / disk quota all
still apply; the microVM is an *additional* boundary.

Three supporting decisions make it safe:

1. **Fail-closed preflight.** When the runtime resolves to `kata` or `krun`,
   fleet preflights the host **before the first container starts** and aborts
   boot on failure — it never silently falls back to a shared-kernel container,
   which would be a silent loss of the requested isolation. The hard gate is
   read-write access to `/dev/kvm` (no usable KVM ⇒ no hypervisor isolation ⇒
   refuse to start) plus the runtime binary on `PATH`; for `krun` the binary
   must report `+LIBKRUN` (a plain `crun` renamed to `krun` would run as an
   ordinary container — the missing feature flag is a hard fail). `kata-runtime
   check` runs only as a **non-fatal warning**: run non-root it skips privileged
   checks and can exit non-zero for reasons that don't mean Kata is unusable, so
   gating on its exit code would break otherwise-healthy rootless-kata hosts.

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
  `internal/sandbox/oci_runtime_test.go` pins all of it, including that
  kata/krun preflight returns an error when KVM or the runtime binary is absent.
- The single production pool-construction path (`agent.buildSandboxPool`) calls
  `sandbox.PreflightRuntime` and returns its error, so a failed preflight aborts
  boot; `fleet validate-config` runs the same check as an operator preflight.
- `TestContainerKataRuntime` exercises a real kata sandbox end-to-end, skipping
  unless the host can actually run it (`/dev/kvm` + `kata-runtime`).

## Consequences

- Operators gain a hypervisor-isolation posture by setting one manifest field,
  at the cost of provisioning `/dev/kvm` + a microVM runtime and tolerating
  slower (~2 s) cold container boots — mitigated by raising the warm-pool depth.
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
