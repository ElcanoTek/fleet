# ADR-0002: Mandatory rootless-Podman sandbox; the host executor never ships

- **Status:** Accepted
- **Date:** 2026-06-28 (documents a decision that predates this record)
- **Deciders:** fleet maintainers

## Context

fleet runs model-authored local actions: bash, Python, and file I/O. It also
brokers fixed native and MCP operations whose credentials and governed stores
must remain host-side. Anything that executes model-chosen commands or general
filesystem operations on the host is a remote-code-execution surface by
construction. A "fast path" that runs such execution directly on the host
"just this once" would quietly become the path that matters, and the security
story would be a fiction.

## Decision

Every agent tool call's **model-authored local execution** runs inside the
**rootless-Podman sandbox**. There is **no** unsandboxed shell, Python, or
general file-operation fast path. The agent loop holds no privileged local
executor of its own; the host validates policy and hands those operations to
the sandbox. The container runs hardened: read-only rootfs, `--cap-drop=ALL`,
`no-new-privileges`, a curated default-deny **seccomp syscall filter** (#219,
`--security-opt seccomp=…`), a same-path workspace bind, and a per-turn network
policy. The seccomp profile is defense-in-depth *beyond* the capability drops:
it withholds high-attack-surface syscalls (`ptrace`, `perf_event_open`,
`keyctl`, `userfaultfd`, the `io_uring` family, `bpf`, `personality`) that
`--cap-drop=ALL` alone does not filter, while still permitting everything
bash / Python / file-I/O legitimately need.

This invariant does not put credentials into the container. MCP calls, guarded
network fetches, governed datastore mutations, approval/email staging, audit
logs, and a bounded set of path-validated legacy workspace staging operations
are fixed host-side brokers/control-plane code, not general local executors.
ADR-0003 defines credential brokering; ADR-0036 inventories and threat-models
the complete native host-I/O exception set. Adding another exception requires
an ADR amendment; it may not be smuggled in as an execution shortcut.

The unsandboxed host executor exists **only** as a test fixture, compiled in
solely under the `fleet_host_executor` build tag (`internal/sandbox/host.go`).
The shipped release binary is built **without** that tag, so
`internal/sandbox/host_disabled.go` stands in and the host executor is not
present in production at all (issue #159).

## Enforcement

- Release builds compile untagged (`go build ./...`), proving `host.go` is
  fenced out and the `host_disabled.go` stub is what ships.
- The sandbox isolation invariants are asserted against a **real** rootless
  Podman in the always-on `e2e-live` CI job, which treats a *skipped* invariant
  test as a failure (a silently-skipped security test is a false green — see
  `.github/workflows/ci.yml`).
- The `podman run` hardening flags are pinned by
  `internal/sandbox/podman_args_test.go`, which substitutes a fake `podman` that
  records its argv and asserts each flag is present: `--read-only`,
  `--cap-drop=ALL`, `--security-opt=no-new-privileges`, `--network=none`, a real
  seccomp profile path (an `unconfined`/`none` value fails the test),
  `--userns=keep-id:…`, the same-path workspace bind + workdir, a positive
  `size=` on every tmpfs, `--memory-swap` equal to `--memory`, `--pids-limit`,
  `--cpus`, and both disk-quota caps. It needs no podman and no image, so it
  runs in the ordinary `go test` lane.
- `internal/sandbox/sandbox_hardened_test.go` covers a different layer and pins
  no flags: it boots a real container to prove the seccomp profile actually
  blocks `ptrace(2)`, and pins the systemd unit's hardening profile (the
  cgroup-migration regression and the `RestrictSUIDSGID=true` canary). All three
  of its tests are opt-in behind `FLEET_SANDBOX_HARDENED_TEST=1`, which **no CI
  lane sets** — they are validated by operator/manual runs only.

## Consequences

- New model-authored local execution capabilities must work *through* the
  sandbox (for example via the FileOp seam or a reviewed fixed container-config
  field), not around it. Arbitrary trailing Podman arguments are deliberately
  not exposed: duplicate or privileged flags could override mandatory
  hardening. This
  shaped how the seccomp filter (#219, now shipped) was added, and shapes how
  features such as an egress allowlist (#211) are added. The seccomp profile defaults ON with the bundled
  default-deny allowlist; operators can point `FLEET_SANDBOX_SECCOMP_PROFILE` at
  a custom OCI profile, or set it to `none` to disable the filter for debugging
  (which logs a warning, since it removes a security layer — the other hardening
  flags still apply).
- Some host-only conveniences are simply unavailable to local execution tools;
  that is the accepted cost of a single, honest isolation boundary. Fixed
  brokers remain narrow, path-validated where applicable, and separately
  audited under ADR-0036.
- Running the real sandbox in CI is expensive (it builds the sandbox image and
  boots rootless Podman), but it is the only way the invariant is actually
  tested rather than asserted.

## Alternatives considered

- **An opt-in "trusted" host fast path.** Rejected: the moment it exists it is
  the path that gets used, and `--cap-drop=ALL` on the other path stops meaning
  anything.
- **Compiling the host executor in but gating it at runtime.** Rejected:
  defence in depth is stronger if the code is not in the release binary at all;
  the build tag makes "not shipped" a compile-time fact, not a runtime check.
