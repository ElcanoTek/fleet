# ADR-0008: Opt-in persistent Python REPL is scoped per-conversation

- **Status:** Accepted
- **Date:** 2026-06-29
- **Deciders:** fleet maintainers

## Context

Issue #213 asks for a persistent Python REPL: an IPython kernel whose
variables, imports, and objects survive **across turns** so a `DataFrame`
built in turn 1 is still in scope in turn 3 without re-reading from disk.

Today every interactive turn takes a fresh rootless-Podman sandbox from the
warm pool and **closes it at turn end** (`internal/sandbox/pool.go`). The kernel
lives inside that sandbox, so its in-memory state dies with the turn. The
documented pool invariant read: *"Pool members are never recycled across turns
— fresh containers for fresh turns. This avoids the cross-conv leak where a
'warm' sandbox carried files from the previous conversation into the next."*

A persistent kernel necessarily keeps a sandbox alive across turns, which on its
face contradicts that invariant. It is also a behavioural change with real blast
radius: long-lived containers hold memory, accumulate kernel state, and need a
correct reaper or they leak. So the decision is not just *how* to keep a kernel
alive, but *whether it should be the default* and *what isolation boundary it is
allowed to cross*. [ADR-0002](0002-mandatory-rootless-podman-sandbox.md) makes
the sandbox mandatory; nothing here may weaken that.

## Decision

We add a persistent Python REPL as an **opt-in, per-conversation** capability,
gated by `FLEET_PYTHON_REPL_MODE`:

- **`per-turn` (the default):** unchanged behaviour. A fresh sandbox + kernel per
  turn, destroyed at turn end. Existing deployments upgrade with no change in
  isolation or resource posture.
- **`persistent`:** one sandbox (and thus one kernel) is kept alive **per
  conversation, keyed by conversation ID**, and reused across that
  conversation's turns. It is **never shared across conversations.**

We also **reword the invariant** the warm pool upholds. The real guarantee was
always narrower than "fresh per turn": *a sandbox is never shared across
conversations.* Per-turn freshness was one way to guarantee it; per-conversation
persistence is a second way that also guarantees it. Conversation IDs are unique
ULIDs that are never reused, so a persistent sandbox cannot bleed into a
different conversation.

Persistent mode is constrained at the single `takeTurnSandbox` call site:

1. **Lockdown turns are always per-turn.** Lockdown's whole point is a fresh,
   network-sealed container; it never borrows a persistent sandbox.
2. **Scheduled runs are never persistent.** They drive `agentcore.Run` through
   the scheduled runner, which owns its own per-run sandbox + git worktree and
   does not pass through `takeTurnSandbox`.
3. **Unknown `FLEET_PYTHON_REPL_MODE` values fail closed to `per-turn`** rather
   than silently keeping a kernel alive.

Lifecycle safety (`internal/sandbox/persistent.go`):

- **Borrow refcount.** Each turn increments `inUse` on take and decrements it on
  its cleanup; the cleanup does **not** close the sandbox. The idle reaper and
  capacity eviction close a sandbox only when `inUse == 0`, so a long turn can
  never have its kernel pulled out from under it (relevant because
  `httpapi.registerTurn` cancels a prior same-conversation turn, and
  cancellation is cooperative — the old turn may briefly overlap the new one).
- **Remove-from-map-then-close, under the lock.** The reaper and
  `ReleaseChatSession` recheck `inUse`/TTL, delete the map entry, and only then
  Close — so a `TakePersistent` racing the reaper can never receive a sandbox
  that is mid-Close.
- **Reclamation on three paths:** an idle-TTL reaper
  (`FLEET_PYTHON_REPL_IDLE_TTL`, default 30m), conversation delete
  (`ReleaseChatSession`, deferred to the last borrow when a turn is in flight),
  and process shutdown (`Pool.Close` drains the map). A session cap
  (`FLEET_PYTHON_REPL_MAX`, default 32) evicts the least-recently-used idle
  session when exceeded.
- **Liveness probe + recreate.** A persistent container that died between turns
  (OOM-kill, host reap) is detected on the next take and replaced, rather than
  wedging the conversation.

Persistent sandboxes are built from the **same `ContainerConfig`** as warm-pool
members, so they inherit the identical cgroup memory/CPU/PID/disk caps — a leaky
kernel is bounded exactly as a per-turn one is.

`FLEET_PYTHON_CELL_TIMEOUT` (independent of mode) sets a host-operator ceiling on
a single cell; the effective per-cell timeout is `min(call timeout, ceiling)`,
clamped in `Sandbox.RunPython` (the bridge receives no `--env`, so the ceiling
cannot live in the kernel).

## Enforcement

- `internal/config/config.go` `normalizePythonREPLMode` fails closed to
  `per-turn`; `TestLoad_PythonREPLModeFailsClosed` pins it.
- `internal/sandbox/persistent_test.go` pins per-conversation reuse, the
  cross-conversation distinctness (isolation), the in-use-survives-reap and
  deferred-close-on-delete refcount behaviour, idle reaping, LRU eviction,
  dead-sandbox recreation, and `Pool.Close` draining.
- `internal/agent/manager.go` `takeTurnSandbox` is the single gate that excludes
  lockdown and empty-conversation turns; scheduled runs structurally never reach
  it (`internal/scheduledrun`).
- `internal/sandbox/cell_timeout_test.go` pins the `min(call, ceiling)` clamp.

## Consequences

- **Default deployments are unchanged.** Persistent mode is off unless an
  operator sets it, so the conservative isolation/resource posture remains the
  default and the existing test suite is unaffected.
- **Within a persistent conversation, state is sticky and must be reasoned
  about.** Variables, imports, monkeypatches, `os.chdir`, and background
  processes persist across turns. That is the feature; `reset_kernel=true` on
  `run_python` gives the agent (or a user) a clean slate on demand.
- **A cancelled or timed-out cell restarts the kernel and loses prior-turn
  state.** Cancellation/timeout tears the bridge down (the response stream can
  no longer be trusted), so the next call starts a fresh kernel. The kernel the
  torn-down bridge orphaned is reaped by the next bridge's startup sweep
  (`reap_stale_kernels`) plus `--init` zombie reaping, so orphans never
  accumulate inside the surviving container. The state loss is documented in the
  `run_python` tool description so the agent keeps durable state on disk.
- **Idle conversations hold a container until the idle TTL or the LRU cap reaps
  it.** The cap + reaper bound this; an operator sizing for persistent mode
  should account for up to `FLEET_PYTHON_REPL_MAX` live sandboxes.
- **Single-process only.** The per-conversation registry lives in one process.
  A future multi-replica deployment would need sticky-by-conversation routing or
  a shared lease; that boundary is documented, not yet solved.

## Alternatives considered

- **Make `persistent` the default (as the issue proposed).** Rejected: it would
  silently change the security and resource profile of every existing
  deployment on upgrade and reverse the default isolation posture. Opt-in keeps
  the conservative choice as the default for a security-conscious self-hosted
  platform.
- **Recycle warm-pool members across turns instead of a separate registry.**
  Rejected: warm members are anonymous and handed to whichever turn takes them,
  so reuse there would risk exactly the cross-conversation leak the invariant
  forbids. A conversation-keyed registry keeps the boundary explicit.
- **Interrupt/kill the kernel on every turn cancellation.** Rejected as
  premature: the per-sandbox mutex already serializes a cancelled cell ahead of
  the next turn (correct, if occasionally slower). Signal-based interrupt across
  the rootless-Podman exec boundary is added only if real stalls appear.
