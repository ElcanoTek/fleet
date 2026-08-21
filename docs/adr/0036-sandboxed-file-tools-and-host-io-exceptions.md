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

At #784's landing, the older truncation spill remained on host `/tmp` and was
therefore no longer readable by sandboxed `view_file`. Issue #793 subsequently
removed that host spill rather than granting the sandbox access to it. One final
model-output boundary now retains only post-governance text through the confined
`RunFileOp` seam, under a fixed-slot workspace lifecycle. Interactive and
isolated-worktree scheduled drivers bind the artifact writer and `view_file` to
the same private root. Shared non-worktree scheduled runs bind the sandbox and
file-tool root but deliberately install no artifact writer because concurrent
tasks do not own that root; their hard model-output cap still applies. See
[`TOOL-OUTPUT-BOUNDARY.md`](../TOOL-OUTPUT-BOUNDARY.md).

**Documented host-side exception classes.** The remaining native tools that do
host I/O are, by design, host-side **control-plane / broker** operations, not
sandbox data-plane execution. They are not agent code execution and must reach
host-brokered credentials/network that by invariant never enter the sandbox:

- **Host network / brokered fetch**: `web_fetch`, `web_search`,
  `tavily_search`, `smart_search`, `download_url` (HTTP fetch),
  `generate_image` (provider API), `fastio_upload` / Fast.io find,
  `browserbase_live_view` (#987 — one authenticated GET to a fixed public
  vendor host that converts a hosted browser session id into a live-view URL
  for a HUMAN; it drives no browser, so ADR-0044's "browser automation is a
  connector" stands. Registered per turn only when a credential is actually
  reachable — the running user's own Browserbase connector key, or a box-wide
  `BROWSERBASE_API_KEY`; see `docs/BROWSERBASE.md`). These use host-side
  credentials and the egress-proxy/allowlist posture; running them in the sandbox would either leak
  credentials in or lose the host broker.
- **Host workspace staging** (path-validated legacy exceptions):
  `fastio_upload` reads bytes for an outbound upload; `publish_artifact` stats
  a confined path and records a pointer rather than opening arbitrary content.
  Neither invokes a shell, dynamic import, or template executor; all
  model-selected paths pass the workspace/pathsec allowlist. This class
  originally also covered `download_url` (writing fetched bytes),
  `generate_image` (reading reference images, writing provider output), and
  `xlsx` (a host zip read/rewrite) — those three were migrated in #1083: they
  are bound to the per-turn sandbox and move every file byte through the
  `RunFileOp` seam (nil sandbox fails closed, no host fallback), while their
  network halves stay host-side under the brokered-fetch class above.
- **Approval/email broker staging**: `email_materialize` reads a size-bounded,
  conversation-confined content file after the send-email approval stager has
  selected it; inline email previews similarly read validated upload/workspace
  attachments. These reads are fixed broker code needed before an approved host
  send, not a general file API. They remain a documented check-then-read host
  exception and are candidates for FileOp migration.
- **Host datastore / control state**: `task_tracker`, `notes`, `memory`, and
  `publish_artifact` write governed database/control records, not arbitrary
  model-selected host files.
- **Admin-authored scheduler gate**: `run_if` executes a shell command before
  task promotion as the Fleet host user. Only an authenticated admin can create
  or change it; the command receives a fixed minimal environment with no Fleet
  credentials, has a 300-second maximum timeout, and retains at most 8 KiB of
  stderr. It is operator configuration, not model-authored execution or a task
  tool, and is deliberately not available to `create_task`.
- **Credential enrollment control plane**: remote-MCP OAuth authorization and
  callback endpoints plus API-key connector intake/probes handle credentials in
  fixed parent-side HTTP code before encrypted storage. They are not
  model-callable. Per-run lookup, refresh, client construction, and MCP calls
  are child-owned under ADR-0040.
- **Host observability/control state**: bash audit and agent audit/session logs
  remain host control-plane state. The former bash/run_python/web_fetch temp
  spills and agent-history overflow breadcrumbs are removed; governed recovery
  bytes are written only through the bound sandbox FileOp capability.

## Consequences

ADR-0002 now states the enforceable boundary precisely: general model-authored
local execution is sandboxed, while the fixed control-plane exceptions above
are documented rather than silently contradicted.
File tools can no longer reach host paths that are not sandbox mounts even when
host validation would allow them; model-output retention is instead rooted in
the effective sandbox workspace;
operator `FLEET_ALLOWED_DIRS` dirs are reachable through the file tools only if
the operator also mounts them into the sandbox (documented). A write under a
read-only supporting-doc mount now fails with an in-container EROFS-style error
instead of a host pathsec error — same containment, different text. Each
container file op adds one short-lived `podman exec` (~50–150 ms); a persistent
in-container fileops session to amortize it is deferred. Governed large-output
recovery uses 16 immutable workspace slots through this same seam (capacity is
never reused within the workspace lifecycle); there is no host-temp fallback.

## Deferred

Migrating the staging/read / compute tools (`xlsx`, `download_url`,
`generate_image`, `fastio_upload`, and email materialization) onto the seam; a
persistent fileops exec session; auto-mounting `FLEET_ALLOWED_DIRS`; and #787's
edit-semantics hardening (unique-match, content hashes, CRLF preservation and a
multi-file patch contract), for which the `FileOpRequest` shape is deliberately
prepared.
