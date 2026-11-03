# ADR-0097: A scheduled job's sandbox mounts only its own directory of the workspace root

Status: accepted; amends ADR-0069 (per-job scheduled workspaces).

## Context

ADR-0069 gave every non-worktree scheduled job its own working directory,
`<workspace-root>/tasks/<lineage_id>/`, and said that in consequence one job's
leftovers never feed another job's reasoning or audit. That held for the
working directory and for the file tools (the fileop anchor refuses a path
outside the run's root). It did not hold for `bash` and `run_python`: the
sandbox bind-mounted the whole workspace root read-write, so `ls ..` from a
job's directory listed every other job, every chat conversation directory and
the whole `mcp-runs/` tree, and `cat` read their files.

Issue #1897 records six bench runs that did exactly that, unprompted, after an
earlier tool result had been trimmed: they read other jobs' result files with
the shells, built their answers from them, passed their self-audit (a file the
run read is evidence in its own history) and finished `success`. On a host
that serves more than one client, that is one client's job reporting another
client's numbers as its own. The mount was read-write, so a run could also
have changed another job's files.

## Decision

A non-worktree scheduled run whose effective workspace is its own job
directory takes a **scoped sandbox** (`sandbox.Pool.TakeScoped`), which
mounts:

- the job directory, read-write, as the working directory and the only
  writable part of the workspace root;
- the pool's read-only mounts (supporting docs, the shared file library, the
  staged skills) **except** the whole `mcp-runs/` tree;
- this occurrence's own `mcp-runs/task-<id>`, read-only, when one of its MCP
  servers uses `${FLEET_WORKSPACE}` (created before the sandbox starts, since
  a mount source must exist).

Nothing else under the root exists inside the container. Both backends
implement it from one mount plan (`ContainerConfig.workspaceMounts`): podman
mounts the job directory at its same path instead of the root; kubernetes
mounts it as a `subPath` of the workspace claim at its same path. The fileop
anchor reads the same plan, so the file tools trust exactly what is mounted.

Every network posture and per-task limit is carried unchanged; the take always
cold-starts, because the warm inventory is spawned with the whole-root mount.
A bad scope fails the take closed; it never degrades to the whole root. Only
a pool with no container backend (host/mock) falls back to `Take`, as every
other scheduled take already does.

The whole-root mount stays for: worktree runs (the worktree's git metadata is
in the root repository's `.git`); `FLEET_SCHEDULED_SHARED_WORKSPACE=1`; a job
directory that could not be created (the run already falls back to the root);
and interactive chat, whose warm pool is unchanged.

## Consequences

- A scheduled job's shells can no longer list, read or write another job's
  directory, a conversation's directory, or another occurrence's MCP run
  directory. ADR-0069's claim now holds for the shells as well as the file
  tools.
- Every non-worktree scheduled run cold-starts its sandbox. Sealed runs on a
  non-lockdown fleet already did (`TakeContainer`); networked runs on an open
  fleet, and every run under fleet-wide lockdown, lose the warm-pool start.
  Scheduled runs are unattended, so the start latency is paid by nobody
  waiting at a screen.
- A job that read another job's output through the root by path can no longer
  do so from the sandbox. It can use the shared file library, a worktree
  checkout, or the operator's `FLEET_SCHEDULED_SHARED_WORKSPACE=1`.
- Not closed here: interactive chat sandboxes still mount the whole root (the
  per-conversation mount ATTACHMENT-SCOPING.md defers, with its warm-pool
  trade-off); worktree runs still see the root; and host-side tools are
  untouched (a connector allowlisting `${FLEET_WORKSPACE_ROOT}` still reads
  under the root, because it runs on the host, not in the sandbox).
