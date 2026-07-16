# ADR-0036: Sandboxed file tools, and the host-side I/O exception classes

Status: accepted (enforces ADR-0002; does not supersede it)

## Context

ADR-0002 states the load-bearing invariant that **every agent tool call runs
inside the rootless-Podman sandbox — there is no unsandboxed fast path**. On
`dev` before this change the model-callable `view_file`, `write_file`, and
`edit_file` tools violated it: they called `os.ReadFile` / `os.WriteFile` /
`os.Open` directly in the Fleet host process (`internal/tools/fs.go`). Host
path validation (`resolveWorkspacePath` / `ValidatePath`, #575) is real
defense-in-depth, but check-then-open validation on the host is a different
security model from executing inside the sandbox: a configured Kata/libkrun
runtime, the read-only rootfs, seccomp, dropped caps, cgroups, PID and disk
limits, and the lockdown network posture all applied to bash/run_python but
**not** to these file operations.

A separate audit (issue #784, point 4) also required an honest classification
of every other native tool that performs host `os.*` / network I/O.

## Decision

**File tools execute in the sandbox.** A new sandbox seam
`Sandbox.RunFileOp` (`internal/sandbox/fileops.go`) dispatches read/write/edit
into the active backend. The container backend runs a one-shot
`podman exec -i python3 -c <embedded fileops.py>` through the same
`podmanArgs` as bash, so the file op inherits the container's runtime,
seccomp, caps, cgroups, disk/PID limits, and network posture by construction;
it deliberately does **not** use the run_python IPython bridge (no kernel boot,
no serialization against run_python, works in lockdown). The host backend
(test/dev only, `fleet_host_executor`) runs the same embedded script via a
plain `python3` subprocess so semantics are byte-identical. The three tools are
bound to the per-turn sandbox exactly like bash/run_python; invoked with a nil
sandbox they **fail closed** — there is no host-execution fallback, and an
untagged release build contains no host file-op implementation at all (the
#159 posture, extended to file I/O). Host-side path validation stays as
INPUT (identical error strings, all traversal/symlink/`..` tests unchanged);
it is no longer the execution mechanism.

Because `view_file` now reads inside the sandbox, it can no longer read a
truncation spill file left on the host `/tmp` (the container's `/tmp` is a
private tmpfs). This PR does **not** relocate the spill: the truncation marker
now steers the model to the `run_python`/`return_vars` (or re-run-filtered)
recovery instead of a `view_file` path, and the on-disk host copy is kept only
as an operator breadcrumb. A sandbox-readable, conversation-scoped,
cleanup-bound full-output artifact is issue #793's job — its acceptance
criteria cover exactly that — so relocating spill here would duplicate and
pre-empt that design (an earlier revision that wrote spills into the workspace
was reverted after review found it polluted the workspace inventory and file
browser and could be git-committed inside a scheduled run's worktree).

The seam preserves an existing file's mode on overwrite/edit (defaulting to
`0600` only when creating), matching the pre-#784 `os.WriteFile` behavior, so a
`chmod +x`'d script keeps its execute bit through an edit.

**Documented host-side exception classes.** The remaining native tools that do
host I/O are, by design, host-side **control-plane / broker** operations, not
sandbox data-plane execution. They are not agent code execution and must reach
host-brokered credentials/network that by invariant never enter the sandbox:

- **Host network / brokered fetch**: `web_fetch`, `web_search`,
  `tavily_search`, `smart_search`, `download_url` (HTTP fetch),
  `generate_image` (provider API), `fastio_upload` / Fast.io find. These use
  host-side credentials and the egress-proxy/allowlist posture; running them in
  the sandbox would either leak credentials in or lose the host broker.
- **Host workspace staging writes** (path-validated): `download_url` and
  `generate_image` write fetched bytes into the bind-mounted workspace;
  `xlsx` composes a spreadsheet host-side. These are candidates for future
  migration onto the FileOp seam (deferred, below).
- **Host datastore / control state**: `task_tracker`, `notes`, `memory`,
  `publish_artifact`, `email_materialize` record governed state through
  host-side stores, not the sandbox filesystem.

## Consequences

The blanket ADR-0002 claim is now true for file I/O, and honest about the
control-plane exceptions above (documented rather than silently contradicted).
File tools can no longer reach host paths that are not sandbox mounts even when
host validation would allow them: host `/tmp` spill is fixed by relocation;
operator `FLEET_ALLOWED_DIRS` dirs are reachable through the file tools only if
the operator also mounts them into the sandbox (documented). A write under a
read-only supporting-doc mount now fails with an in-container EROFS-style error
instead of a host pathsec error — same containment, different text. Each
container file op adds one short-lived `podman exec` (~50–150 ms); a persistent
in-container fileops session to amortize it is deferred. The truncation spill
stays on host `/tmp` and is no longer agent-readable; recovery is via
`run_python`/re-run (see #793 for the sandbox-readable artifact).

## Deferred

Migrating the staging-write / compute tools (`xlsx`, `download_url` and
`generate_image` workspace writes, `fastio_upload` reads) onto the seam; a
persistent fileops exec session; auto-mounting `FLEET_ALLOWED_DIRS`; and #787's
edit-semantics hardening (unique-match, content hashes, mode/CRLF preservation,
multi-file patch), for which the `FileOpRequest` shape is deliberately prepared.
