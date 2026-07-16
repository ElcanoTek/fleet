# ADR-0036: Sandboxed file tools, and the host-side I/O exception classes

Status: accepted (enforces and narrows ADR-0002's blanket wording)

## Context

ADR-0002 states the load-bearing invariant that **model-authored local
execution runs inside the rootless-Podman sandbox — there is no unsandboxed
executor fast path**. Its original blanket "every tool call" wording was
inaccurate for intentionally host-brokered MCP and native operations; ADR-0003
already requires MCP credentials to stay host-side. On
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

The container mounts the shared workspace root for bash/Python compatibility,
so the FileOp protocol also carries the *narrow* per-conversation or scheduled
worktree root. Immediately after sandbox assignment, `BindFileOpRoot` captures
that root's device/inode identity **inside the selected runtime**. Every later
writable FileOp opens the root relative to a trusted bind-mount anchor, rejects
an identity mismatch, then walks all remaining components with directory
descriptors and `O_NOFOLLOW`; open and replace are descriptor-relative. This
closes both interior symlink swaps and whole conversation-directory exchanges.
Supporting-document mounts form separate read-only FileOp capabilities and
cannot be selected for write/edit. Atomic replacement fsyncs the file and
parent directory, creates new directories as exact `0750` and new files as
`0600`, and preserves an existing file's mode and ownership when the rootless
mapping permits. If a FileOp is cancelled or reaches its 60-second ceiling,
Fleet synchronously SIGKILLs the whole container and poisons/retires it, as for
bash (#796); killing only the host-side `podman exec` client would allow a
helper to finish a late rename.

Because `view_file` now reads inside the sandbox, it cannot read a truncation
spill left on host `/tmp` (the container has a private `/tmp`). This PR does
not pretend otherwise: the marker directs the model to re-run with filtering or
capture through `run_python`, while the host copy is an operator breadcrumb
swept after 24 hours. Issue #793 owns a bounded, conversation-scoped,
sandbox-readable artifact lifecycle; moving spill files ad hoc here would
pre-empt its inventory and retention design.

**Documented host-side exception classes.** The remaining native tools that do
host I/O are, by design, host-side **control-plane / broker** operations, not
sandbox data-plane execution. They are not agent code execution and must reach
host-brokered credentials/network that by invariant never enter the sandbox:

- **Host network / brokered fetch**: `web_fetch`, `web_search`,
  `tavily_search`, `smart_search`, `download_url` (HTTP fetch),
  `generate_image` (provider API), `fastio_upload` / Fast.io find. These use
  host-side credentials and the egress-proxy/allowlist posture; running them in
  the sandbox would either leak credentials in or lose the host broker.
- **Host workspace staging** (path-validated legacy exceptions):
  `download_url` writes fetched bytes; `generate_image` reads reference images
  and writes provider output; `fastio_upload` reads bytes for an outbound
  upload; `xlsx` performs a fixed-schema, pure-library spreadsheet transform.
  `publish_artifact` stats a confined path and records a pointer rather than
  opening arbitrary content. None invokes a shell, dynamic import, or template
  executor; all model-selected paths pass the workspace/pathsec allowlist.
- **Approval/email broker staging**: `email_materialize` reads a size-bounded,
  conversation-confined content file after the send-email approval stager has
  selected it; inline email previews similarly read validated upload/workspace
  attachments. These reads are fixed broker code needed before an approved host
  send, not a general file API. They remain a documented check-then-read host
  exception and are candidates for FileOp migration.
- **Host datastore / control state**: `task_tracker`, `notes`, `memory`, and
  `publish_artifact` write governed database/control records, not arbitrary
  model-selected host files.
- **Host observability and temporary spill**: bash writes its operator audit
  log; bash/run_python/web_fetch currently write per-capture-size-limited,
  24-hour-swept truncation files to host temp, and the agent history compactor
  writes overflow breadcrumbs beneath its configured data root. These are fixed
  post-processing paths, never code execution, but there is no aggregate byte
  quota in this pre-#793 implementation. #793 removes the model-output spill
  exception in favor of a quota/retention-governed sandbox artifact lifecycle;
  audit/session logs intentionally remain host control-plane state.

## Consequences

ADR-0002 now states the enforceable boundary precisely: general model-authored
local execution is sandboxed, while the fixed control-plane exceptions above
are documented rather than silently contradicted.
File tools can no longer reach host paths that are not sandbox mounts even when
host validation would allow them. In particular, the current host `/tmp` spill
is not agent-readable (the #793 artifact contract replaces it); operator
`FLEET_ALLOWED_DIRS` dirs are reachable through the file tools only if
the operator also mounts them into the sandbox (documented). A write under a
read-only supporting-doc mount now fails with an in-container EROFS-style error
instead of a host pathsec error — same containment, different text. Each
container file op adds one short-lived `podman exec` (~50–150 ms); a persistent
in-container fileops session to amortize it is deferred. The truncation spill
stays on host `/tmp` and is no longer agent-readable; recovery is via
`run_python`/re-run (see #793 for the sandbox-readable artifact).

## Deferred

Migrating the staging/read / compute tools (`xlsx`, `download_url`,
`generate_image`, `fastio_upload`, and email materialization) onto the seam; a
persistent fileops exec session; auto-mounting `FLEET_ALLOWED_DIRS`; and #787's
edit-semantics hardening (unique-match, content hashes, CRLF preservation and a
multi-file patch contract), for which the `FileOpRequest` shape is deliberately
prepared.
