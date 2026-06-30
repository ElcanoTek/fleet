# Sandbox OCI runtimes — runc · Kata · libkrun

Every agent tool call (`bash`, `run_python`, file I/O, MCP) runs inside the
**mandatory** rootless-Podman sandbox — there is no fast path that skips it
([ADR-0002](adr/0002-mandatory-rootless-podman-sandbox.md)). What this guide
covers is the *isolation posture* of that sandbox: which OCI runtime Podman
executes the container under, selected per deployment without replacing Podman
or changing any other invariant.

The runtime is chosen with one knob — the manifest's `sandbox.runtime` (or the
`FLEET_SANDBOX_RUNTIME` env var, which overrides it). The flag flows straight
through to `podman run --runtime=<value>`; fleet adds a fail-closed boot
preflight and, for Kata, a guest-memory adjustment. See
[ADR-0010](adr/0010-microvm-sandbox-runtimes.md) for the design rationale.

## The three tiers

| | **runc / crun** (default) | **Kata Containers** | **libkrun** |
|---|---|---|---|
| `sandbox.runtime` | `""` (or `runc`) | `kata` | `libkrun` (→ `krun`) |
| Kernel | **Shared** with the host | **Separate** guest kernel | **Separate** guest kernel |
| Boundary | namespaces + cgroups + seccomp | **KVM hypervisor** (QEMU / Cloud Hypervisor) | **KVM hypervisor** (in-process VMM) |
| Escape requires | a container-escape / kernel CVE | a **hypervisor** CVE | a **hypervisor** CVE |
| Boot overhead | ~100 ms | ~2 s | ~0.5–1 s |
| Memory overhead | none | ~512 MiB guest baseline | tens of MiB |
| Host requirement | none | `/dev/kvm` + `kata-runtime` | `/dev/kvm` + `krun` (`+LIBKRUN`) |
| Podman compatibility | native | drop-in `--runtime` | drop-in `--runtime` |

**Pick `runc`/default** for trusted workloads where a container boundary is
enough. **Pick `kata` or `libkrun`** when you run untrusted prompts or sensitive
data and want a kernel-level break to require breaking the hypervisor, not just
the container — the Suna/Daytona microVM posture, without new infrastructure.

`kata` is the most battle-tested microVM runtime; `libkrun` is lighter (no
separate QEMU process) but has thinner ecosystem support for the guest agent.

## Configuration

In the client bundle's `manifest.yaml`:

```yaml
sandbox:
  containerfile: sandbox/Containerfile
  tag: localhost/fleet-sandbox:latest
  runtime: kata        # "" | runc | kata | libkrun | runsc | <absolute path>
```

Precedence (mirrors `sandbox.image`): an explicit **`FLEET_SANDBOX_RUNTIME` env
var wins**, else the manifest's `sandbox.runtime`, else Podman's configured
default. Both the env var and the manifest are operator-authored deployment
config (the manifest already pins the sandbox image and Containerfile), so the
runtime selection is a trusted operator choice.

Name handling:

- **`libkrun`** is the product name; Podman's registered runtime is **`krun`**
  (a `crun` build with `+LIBKRUN`). fleet normalizes `libkrun` → `krun`
  automatically and logs the rewrite. You may also write `krun` directly.
- A value containing a path separator (e.g. `/opt/kata/bin/kata-runtime`) is
  passed to `--runtime` **verbatim** — fleet never rewrites an explicit path —
  but it is still classified by its **basename** for the preflight and memory
  overhead, so a path pointing at kata/krun gets the same fail-closed KVM gate as
  the bare name (no silent bypass).
- Bare names are lower-cased.

Verify a host before going live:

```sh
fleet validate-config
```

The `sandbox` check confirms podman is reachable, the image is present, the
runtime binary is on `PATH`, and — for `kata`/`krun` — runs the same fail-closed
KVM preflight the boot path runs.

## Fail-closed preflight

When the runtime resolves to `kata` or `krun`, fleet runs a preflight **before
the warm pool spawns its first container**. A failure **aborts startup** — fleet
never silently falls back to a shared-kernel container, because that would be a
silent loss of the isolation you asked for (the no-degrade invariant).

**Kata** — fails closed unless:
- `kata-runtime` is on `PATH`, and
- `/dev/kvm` opens **read-write** (the fleet user must be in the `kvm` group).

`kata-runtime check --no-network-checks` runs too, but only as a **non-fatal
warning** — run non-root it skips privileged checks and can exit non-zero for
reasons that don't mean Kata is unusable, so its exit code is logged, not gated
on. `/dev/kvm` is the real gate.

**libkrun / krun** — fails closed unless:
- `/dev/kvm` opens read-write,
- `krun` is on `PATH`, and
- `krun --version` reports `+LIBKRUN`. A plain `crun` renamed to `krun` would
  run as an ordinary shared-kernel container — the missing feature flag is a
  hard fail so that downgrade can't pass silently.

## Host setup

### 1. KVM access (kata and libkrun)

Both microVM runtimes boot via KVM, so the fleet process user needs read-write
access to `/dev/kvm`. On most distros that means membership in the `kvm` group:

```sh
getent group kvm                 # confirm the group exists
sudo usermod -aG kvm fleet       # add the fleet service user
```

If fleet runs as a **systemd** unit, make the unit pick up the supplementary
group (a running user manager may need a re-login or restart to see a new group
membership):

```ini
[Service]
SupplementaryGroups=kvm
```

Verify as the fleet user:

```sh
sudo -u fleet test -r /dev/kvm && sudo -u fleet test -w /dev/kvm && echo "kvm OK"
sudo -u fleet --preserve-env=HOME,XDG_RUNTIME_DIR podman info >/dev/null && echo "podman OK"
```

> **`/dev/kvm` must be genuinely usable.** Nested virtualization that doesn't
> expose KVM passthrough, a device-cgroup policy, or wrong permissions can make
> the node exist but be unusable. `/dev/kvm` is available on bare metal and most
> cloud VMs; it is **not** available in nested-virt environments unless the
> outer hypervisor exposes KVM.

### 2. Install Kata Containers

Install Kata so `kata-runtime` is on `PATH` and Podman's `containers.conf`
registers the `kata` runtime (recent Podman ships the registration by default in
`[engine.runtimes]`). Confirm:

```sh
kata-runtime check --no-network-checks
podman run --rm --runtime=kata --network=none <your-sandbox-image> true && echo "kata OK"
```

See the upstream
[Kata + Podman guide](https://github.com/kata-containers/kata-containers/tree/main/docs/how-to).

### 3. Install libkrun (krun)

On Fedora the stock `crun` is built with libkrun support and ships a `krun`
entrypoint; otherwise install a `crun`/`krun` built with `+LIBKRUN`. Confirm:

```sh
krun --version | grep -q '+LIBKRUN' && echo "krun has libkrun support"
podman run --rm --runtime=krun --network=none <your-sandbox-image> true && echo "krun OK"
```

See [containers/libkrun](https://github.com/containers/libkrun).

### 4. cgroup v2 delegation (memory limits)

fleet caps each sandbox's memory via `--memory`. On a rootless host **without
cgroup v2 delegation**, Podman can silently ignore `--memory` — the per-task
caps (and the Kata overhead below) then don't bind. Confirm:

```sh
podman info --format '{{.Host.CgroupsVersion}}'   # want: v2
```

## Kata memory overhead

A Kata VM carries ~512 MiB of guest-kernel + VMM baseline. So that
`FLEET_SANDBOX_MEMORY` (and per-task `sandbox_limits.memory_mb`) still reflect
**usable guest** memory, fleet adds that overhead to the container's `--memory`
ceiling when `runtime=kata`:

```
--memory = ceil(configured limit to MiB) + FLEET_SANDBOX_KATA_OVERHEAD_MB
```

| Configured limit | Container `--memory` (kata, default overhead) |
|---|---|
| `512m` | `1024m` |
| `2g` | `2560m` |
| `2048m` (per-task) | `2560m` |

- **`FLEET_SANDBOX_KATA_OVERHEAD_MB`** overrides the default `512`. An invalid
  value is ignored (logged) and the default stands.
- The overhead is **kata-only** — libkrun's footprint is an order of magnitude
  smaller, so no bump is applied to `krun`.
- The overhead is added **on top of** the per-task ceiling. A task at
  `FLEET_SANDBOX_MEMORY_MAX_MB` (8192) under kata is allocated `8192 + overhead`
  MiB of host RAM — the ceiling bounds **usable guest** memory, so size host RAM
  to include the overhead.
- An **unparseable or overflowing** memory limit fails closed under kata (rather
  than booting a guest that may be too small). A bare number is interpreted as
  **bytes**, and `k`/`m`/`g` suffixes are powers of 1024 — matching Podman.

> **The node's Kata config can be the binding constraint.** Raising Podman's
> `--memory` only lifts the *ceiling*. If `default_memory` in the node's Kata
> `configuration.toml` caps guest RAM lower, the extra memory never reaches the
> guest. Coordinate the overhead with your Kata node config.

## Warm pool sizing

The warm pool ([#181](adr/)) works identically across runtimes, but Kata
containers boot slower (~2 s vs. ~100 ms for runc), so a cold first turn costs
more. Raise the warm-pool depth so more containers stay pre-booted:

```sh
FLEET_SANDBOX_WARM_SIZE=3   # kata users: at least 2–3
```

(Without an explicit value the depth is derived from
`FLEET_MAX_CONCURRENT_AGENTS`, clamped to `[2, 8]`.)

## What does not change

Switching runtime changes only how the OCI runtime executes the container.
Everything else is identical:

- The credential broker is unchanged — MCP/connector credentials stay host-side
  and never enter the sandbox, regardless of runtime
  ([ADR-0003](adr/0003-host-side-mcp-credential-brokering.md)).
- `bash` / `run_python` / file I/O behave the same; `podman exec` into the
  container uses the runtime it was created with.
- The seccomp profile, dropped capabilities, read-only rootfs, no-new-privileges,
  network sealing (`--network=none` for lockdown / scheduled runs), disk quota,
  and per-task `sandbox_limits` all still apply — the microVM is an *additional*
  boundary, not a replacement for any of them.
