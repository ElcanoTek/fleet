# Scheduled sandboxes mount only their own job directory

Design note for issue #1897. The decision record is
[ADR-0097](adr/0097-scheduled-sandbox-mounts-only-its-job-directory.md).

## What shipped

A non-worktree scheduled run's `bash` and `run_python` see only the job's own
directory of the workspace root (`tasks/<lineage_id>/`, ADR-0069), plus the
read-only trees every sandbox needs. Concretely, `scheduledrun.runSandboxScope`
builds a `sandbox.WorkspaceScope` and `takeTaskSandbox` routes the run through
`sandbox.Pool.TakeScoped`, which cold-starts a container (podman) or pod
(kubernetes) with this mount set:

| Path | Access | Why |
|---|---|---|
| `<root>/tasks/<lineage_id>/` | read-write, working directory | The job's own workspace. |
| supporting docs (personas, protocols, system prompts, skills) | read-only | Unchanged; the seeded symlinks resolve through them. |
| `<root>/shared/` (and `<root>/skills/` on kubernetes) | read-only | The shared file library and staged skills, as before. |
| `<root>/mcp-runs/task-<id>/` | read-only | This occurrence's MCP ledger and staged inputs, only when one of its servers uses `${FLEET_WORKSPACE}`. |

Not mounted: the root itself, every other `tasks/<lineage>/`, every chat
conversation directory, `mcp-shared/`, and every other occurrence's
`mcp-runs/task-<id>/`. `ls ..` from the job directory therefore shows only the
mount points above.

The podman argument list, the kubernetes pod spec and the fileop anchor all
derive from one function (`ContainerConfig.workspaceMounts`), so the file tools
trust exactly what the shells can reach.

## Failure behaviour

- The scope is validated before anything starts (`resolveWorkspaceScope`): the
  directory must exist and lie strictly inside the workspace root; a read-only
  extra must lie inside the root and must not contain the job directory. Any
  violation fails the take — it never falls back to mounting the whole root.
- **No symlinks below the root.** Everything under the workspace root is
  writable by a whole-root sandbox (a chat, a worktree run), and podman
  follows a symlinked volume source, so a `tasks/<lineage>` swapped for a
  symlink would otherwise mount — and relabel — whatever it points at. The job
  directory and each read-only extra are walked from the root one component
  at a time through pinned descriptors (`openat(…, O_NOFOLLOW|O_DIRECTORY)`),
  so a symlink or non-directory at any component fails the take, and a parent
  renamed to a symlink mid-walk cannot redirect it. The root itself is
  operator configuration and may be a symlink.
- **The mounted directory is the one checked (podman).** To close the window
  between that check and podman's mount, the take `stat`s the job directory
  inside the started container and compares its device and inode with those
  the pinned walk read (`fstat` on the final descriptor); a mismatch tears the
  container down and fails the take. One residual: podman's `:z` relabel runs
  at mount time, so a directory swapped in during that window is relabelled
  before the check refuses the sandbox — it is never handed to a run. The
  kubernetes backend does not run the in-container check — a pod's view of
  the claim is another host's mount, where device and inode numbers are not
  guaranteed to match — and relies on the pinned walk plus the kubelet's own
  `subPath` handling, which refuses a symlink that leaves the volume (a swap
  to another directory inside the claim during pod start is the residual
  there).
- Allowlisted egress without a proxy, and allowlisted egress on kubernetes,
  fail closed exactly as the unscoped takes do.
- A pool with no container backend (host/mock test pools) answers
  `ErrContainerUnavailable` and the run falls back to `Take`, the same
  fallback every scheduled take already had.

## What deviated from the issue

The issue suggested either scoping the mount or correcting ADR-0069 and
AGENT-RUNTIME.md. This change scopes the mount for scheduled runs and corrects
the docs for what remains.

`mcp-shared/` is not mounted into a scoped sandbox. A scheduled run's MCP
servers run on a dedicated per-run client whose `${FLEET_WORKSPACE}` is the
per-occurrence `mcp-runs/task-<id>/`, not `mcp-shared/`, so a scheduled run has
no path there to follow.

## Deliberately deferred

- **Interactive chat.** Chat sandboxes still bind-mount the whole root; see
  [ATTACHMENT-SCOPING.md](ATTACHMENT-SCOPING.md#what-this-does-not-fix). Scoping
  them means giving up the warm pool for chat turns, which is a latency
  decision this change does not make.
- **Worktree runs.** A `worktree_config` run keeps the whole-root mount: the
  workspace root is the git repository, and the worktree's `.git` file points
  into the root's `.git/worktrees/`, which a scoped mount would hide.
- **`FLEET_SCHEDULED_SHARED_WORKSPACE=1`.** The operator asked for the shared
  root, so the run gets the whole-root mount.

## Cost

Every non-worktree scheduled run now cold-starts its sandbox. Sealed runs on a
non-lockdown fleet already did; a networked run on an open fleet, and every
run under fleet-wide lockdown, lose the warm start. Scheduled runs are
unattended, so nobody waits on that latency.

## Tests

- `internal/sandbox/workspace_scope_test.go`: scope validation and refusals,
  the mount plan, the fileop anchor refusing sibling jobs, conversations and
  other occurrences' run dirs, the podman argument list (fake podman), and the
  kubernetes pod spec (`subPath` mounts).
- `internal/scheduledrun`: `TestTakeTaskSandbox_Scoped` (every posture goes
  through `TakeScoped`, never the warm pool) and `TestRunSandboxScope` (which
  runs get a scope, the withheld `mcp-runs/`, and the run dir created before
  the mount only when a server uses it).
