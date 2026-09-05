# Reclamation, disk backpressure, and stuck-task backstops

Everything fleet reclaims on its own, what drives each reclaimer, and what the
box does when it runs out of disk anyway.

The short version: there is **one in-process maintenance loop** (hourly) that
reclaims fleet's own data, **one systemd timer** (daily) that prunes podman's
image store, and **one disk guard** that sheds background load when the
filesystem gets tight. Every mechanism below already had an implementation
before this change; what most of them lacked was anything that *ran* them.

## Why this exists

Reclamation used to be a side effect of chat traffic. The database retention
sweeps, the attachment sweep and the orphan-workspace sweep were called from
exactly one place — the tail of a completed chat turn — which was wrong in both
directions at once:

- **An idle box never reclaimed.** A scheduler-only deployment, or any box whose
  chat went quiet for a week, grew its turn ledgers, expired conversations,
  terminal input-queue rows and orphaned workspace directories without bound.
- **A busy box reclaimed far too often.** Ten concurrent turns each finished by
  running three global database sweeps plus a full recursive walk of the
  attachment tree, inline on the turn goroutine — the same work, N times an
  hour, contending on the same rows.

Two reclaimers had no automatic caller at all: `fleet worktree prune` and
`fleet cleanup` were reachable only by an operator typing them. And nothing
watched free disk: `hoststats` collected the numbers, but only to render them in
an admin panel — no metric, no alert, no action.

## The maintenance loop

`startMaintenanceLoop` (`cmd/fleet/main.go`) runs one pass every hour, bound to
the shutdown context. Each step is independent and best-effort: one failing logs
and the rest still run. The whole pass is bounded by a 15-minute timeout so a
pathological filesystem cannot wedge the loop and silently stop every later
iteration.

| Step | Reclaims | Bound |
| --- | --- | --- |
| `SweepExpired` | Conversations past `CONVERSATION_TTL_DAYS` (default 14; no `FLEET_` prefix — it predates the alias chain), plus per-user cap eviction | TTL / cap |
| `PurgeTerminalInputs` | Terminal input-queue rows | `FLEET_INPUT_QUEUE_RETENTION_DAYS` (30) |
| `SweepTurnEvents` | Finished turns' durable SSE ledgers | `FLEET_TURN_EVENT_RETENTION_DAYS` (14) |
| `SweepAttachments` | Attachment files past the conversation TTL | conversation TTL |
| `SweepOrphanWorkspaces` | `<workspace>/<convID>/` dirs with no live conversation row | liveness, not age |
| `CleanupTempFiles` | Orchestrator `temp_uploads` staging files | conversation TTL |
| `SweepExpiredOAuthFlows` | Abandoned per-user remote-MCP OAuth flow rows | flow expiry |
| `worktree.PruneStale` | Orphaned per-run git worktrees | `FLEET_WORKTREE_PRUNE_AGE` (24h) |

Each store method treats a non-positive TTL as *disabled*, so turning a
retention knob off yields a no-op rather than a surprise deletion.

### The post-turn pass is now an optimization

A completed chat turn still triggers the same pass — it reclaims promptly after
the turn that produced the garbage — but behind a rate gate
(`FLEET_MAINTENANCE_MIN_INTERVAL`, default 5m). The gate is a compare-and-swap
on a single timestamp, so N turns finishing at the same instant cannot all pass
it: exactly one wins the interval. The hourly ticker reports its own passes
through the same timestamp, so the two drivers do not double the work.

Set `FLEET_MAINTENANCE_MIN_INTERVAL=0` to restore the every-turn behaviour.

### What the loop deliberately does NOT do

**Prune podman's image store.** Every sandbox Containerfile change strands the
previous image's layers (~1.3 GB), so this genuinely needs doing — but a
whole-store prune's blast radius (a concurrent build, another instance's images)
belongs to an operator-scheduled window, not to a background goroutine inside
the serving process. It runs from `deploy/fleet-maintenance.timer` instead,
daily at 03:30, as `fleet cleanup`.

`scripts/bootstrap.sh --enable-service` installs and enables that timer by
default; `--no-maintenance-timer` opts out. On a box provisioned before the
timer shipped (or after an opt-out you've changed your mind about), install it
in one command with `sudo fleet timers install --maintenance` — `fleet update`
also offers it interactively when the pair is missing, and `--no-timers`
silences that offer for boxes that deliberately run without it (see
[docs/TIMERS.md](TIMERS.md)). `fleet doctor` checks that it is installed,
enabled, active, and that its last run succeeded — the same posture it applies
to the backup timer.

`fleet cleanup --deep` additionally prunes unused *named* images and is
deliberately left to a human: on a stopped box it can remove the sandbox image
itself and force a rebuild on the next deploy.

## Disk backpressure

`internal/diskguard` measures free space on the filesystem holding the data
directory and decides whether the box should shed load. Below
`FLEET_DISK_MIN_FREE_PERCENT` (default 5) the scheduler stops **claiming**
scheduled tasks and `/readyz` reports the `disk` check as `degraded` (HTTP 207).

The asymmetry is the design:

- **Scheduled work stops.** A full disk on this box is nearly always produced by
  unattended runs, so stopping them is the effective remedy.
- **Interactive chat keeps working.** Chat is the interface an operator uses to
  diagnose and fix the problem. Gating it would take away the tool at the exact
  moment it is needed.
- **Running tasks are untouched.** Shedding stops the queue from draining; it
  never kills a run.

Two properties are load-bearing:

- **It fails open.** A statfs that errors, or a path that does not exist, is
  reported as unavailable and never as below-the-floor. Refusing to run
  scheduled work because we could not *measure* the disk would turn a monitoring
  fault into an outage.
- **It has hysteresis.** Once shedding, free space must climb 2 percentage
  points above the floor before work resumes, so a sweep that frees a sliver
  cannot restart a fill-shed-fill cycle.

It measures the **nearest existing ancestor** of the data dir rather than the
data dir itself, because fleet creates that tree lazily — otherwise a fresh box
would report "unmeasurable" and warn at every boot until something happened to
write there. An ancestor is on the same filesystem in the normal case, and the
reported path names whatever was actually measured. The exception is a data dir
on its own mount that has not been mounted yet: the parent then describes a
different filesystem.

Set `FLEET_DISK_MIN_FREE_PERCENT=0` to disable the decision. The guard still
samples, so the metrics and the admin panel keep working.

### Why `/healthz` stays 200

`/healthz` is the load-balancer gate: a 503 there drains chat traffic off the
box. A shedding box is precisely one whose chat is fine — so draining it would
remove the interface an operator needs to reclaim the space, which is the
opposite of the guard's purpose. The signal therefore lives where a
non-critical degradation belongs:

- `/readyz` → the `disk` check reports `degraded`, folding the overall status to
  `degraded` / **207**, never `not_ready` / 503.
- `fleet_disk_shedding` → the metric to alert on.
- `GET /admin/health` → the numbers behind the decision.

(The guard fails open, so an unmeasurable filesystem reports `ok` on `/readyz`
with the reason in the detail: the probe must not claim a degradation the guard
itself does not act on.)

### Observability

| Metric | Meaning |
| --- | --- |
| `fleet_disk_total_bytes` | Capacity of the data dir's filesystem |
| `fleet_disk_free_bytes` | Free bytes available to the unprivileged service user (statfs `Bavail`) |
| `fleet_disk_free_ratio` | Free space as a fraction 0–1 |
| `fleet_disk_shedding` | `1` when scheduled claims are being held back |
| `fleet_goroutines` | Live goroutines |
| `fleet_memory_heap_bytes` | Allocated heap objects |
| `fleet_memory_sys_bytes` | Total memory obtained from the OS |

`fleet_disk_shedding == 1` is the alert. It is not a prediction that the disk
might fill: it is the box reporting that it has **already** stopped claiming
work. Because chat still serves, the symptom an operator would otherwise notice
unaided is "the queue stopped draining" with no error anywhere.

Pair it with a slope alert on `fleet_disk_free_ratio` to hear about the trend
hours earlier. The Grafana dashboard's **Host resources** row charts all of
these; see [`deploy/grafana/README.md`](../deploy/grafana/README.md).

The Go runtime gauges are a deliberate, minimal set — the handful of numbers
worth trending on a single-box service — not a re-implementation of the standard
Go collector, which fleet's hand-rolled exporter does not ship.
`GOMEMLIMIT` is documented (commented out) in `deploy/fleet.service`: it is a
soft ceiling that makes the GC work harder near the limit, turning an OOM kill
that takes every in-flight turn with it into more frequent GC.

## Stuck-task backstops

A task can stop making progress in several ways. Each has a bound:

| State | Backstop | Bound |
| --- | --- | --- |
| `running`, process crashed | `RecoverExpiredLeases` re-queues; past the retry budget it dead-letters | 5m lease |
| `running`, agent hung | Per-run wall-clock deadline | `FLEET_TASK_WALL_TIMEOUT` (4h) |
| `running`, lease stolen | `errTaskLeaseLost` cancels the stale run | immediate |
| `paused_awaiting_input` | `ExpirePausedTasks` fails it terminally | `FLEET_PAUSED_TASK_EXPIRY_MINUTES` (off by default) |
| `paused_awaiting_wake`, unreachable | `ExpireStrandedWakeTasks` fails it terminally | 24h past the deadline |
| Interactive turn hung | Per-turn context deadline | `FLEET_TURN_TIMEOUT_SECONDS` (default 1800; legacy alias `CHAT_TURN_TIMEOUT_SECONDS`) |

`paused_awaiting_wake` was the gap. `WakeDueTasks` filters on
`wake_at IS NOT NULL`, so a row without one can never wake — and
`ExpirePausedTasks` covers `paused_awaiting_input` only, so nothing failed it
either. It waited forever, with no terminal record and no operator signal.
`ExpireStrandedWakeTasks` now fails two shapes, both anchored on `paused_at` so
a task legitimately sleeping 30 days out is never touched:

1. `wake_at IS NULL` — unreachable by the wake sweep by construction.
2. `wake_at` more than 24h in the past — the wake sweep runs every tick, so this
   far overdue means it is not reaching the row.

It runs **after** the wake sweep on each scheduler tick, so a row that is merely
due gets woken on that same tick and is never a candidate. Like the
awaiting-input expiry, it preserves the recurrence chain: a stranded occurrence
of a daily task spawns its successor rather than silently ending the schedule.

There is no disable knob. An unreachable parked row is a broken row, not an
operator policy choice.

## Sandbox container bounds

Persistent per-conversation REPL sandboxes (`FLEET_PYTHON_REPL_MODE=persistent`)
are bounded by an idle TTL and a session cap. The cap is now enforced by the
**idle reaper** as well as the create path.

That matters because eviction skips sessions with a turn in flight. A burst that
overshot the cap while every other session was busy stayed overshot — nothing
revisited the decision once those turns finished — until the idle TTL expired or
another create happened to arrive. Since the cap is what bounds the box's
container memory, "eventually, if someone starts another conversation" is not a
bound. The reaper runs at most a minute apart, so an overshoot now self-corrects.

It remains a **soft** cap by design: a session with a turn in flight is never
evicted, because pulling a sandbox out from under a running turn would destroy
work. The live count can exceed the limit while those turns run; it just no
longer stays over it afterwards.

At boot, fleet warns when `FLEET_PYTHON_REPL_MAX × FLEET_SANDBOX_MEMORY` claims
more than two thirds of host RAM. The defaults multiply out to 32 × 512 MiB =
16 GiB before the fleet process, Postgres, or the warm pool have taken a byte.
The warning is advisory — a box may legitimately be sized for it, and containers
rarely reach their cap — it just makes the number visible at boot rather than at
3am.

## Configuration reference

| Env var | Default | Effect |
| --- | --- | --- |
| `FLEET_DISK_MIN_FREE_PERCENT` | `5` | Free-space floor below which scheduled claims pause. `0` disables. |
| `FLEET_WORKTREE_PRUNE_AGE` | `24h` | Age before an orphaned worktree dir is reclaimed. `0` disables. |
| `FLEET_MAINTENANCE_MIN_INTERVAL` | `5m` | Rate limit on the opportunistic post-turn pass. `0` = every turn. |
| `FLEET_CLEANUP_HOUR` | `4` | UTC hour of the scheduler's daily run-history retention sweep. |
| `FLEET_RUN_LOG_RETENTION_DAYS` | `90` | Age before terminal task runs are pruned. `0` disables. |
| `FLEET_INPUT_QUEUE_RETENTION_DAYS` | `30` | Age before terminal input-queue rows are purged. `0` disables. |
| `FLEET_TURN_EVENT_RETENTION_DAYS` | `14` | Age before finished turns' SSE ledgers are swept. `0` disables. |
| `FLEET_PAUSED_TASK_EXPIRY_MINUTES` | `0` (off) | Fail a task awaiting input longer than this. |
| `FLEET_TASK_WALL_TIMEOUT` | `4h` | Per-run wall-clock ceiling. `0` disables. |
| `FLEET_PYTHON_REPL_MAX` | `32` | Persistent sandbox session cap (soft). `0` = unbounded. |
| `FLEET_PYTHON_REPL_IDLE_TTL` | `30m` | Idle time before a persistent sandbox is reaped. |

## Honest scope

What this does **not** do:

- **No notification on an expiry.** Neither expiry sweep fires the
  task-completion notification the runner sends on a normal terminal failure —
  the notifier lives in the runner, not the scheduler. This is a pre-existing
  gap, unchanged here and called out in `Storage.ExpirePausedTasks`.
- **The disk guard measures one filesystem.** The data dir's. A box whose podman
  image store is on a *different* mount gets no backpressure from that mount
  filling — `fleet doctor` checks it, and the daily prune targets it, but the
  runtime guard does not watch it.
- **`fleet_disk_shedding` is not exported per mount.** One fleet process, one
  data directory, one series.
- **The worktree sweep is age-based, with two guards.** Age alone cannot tell
  a crashed run's worktree from a running task's, which is why the default
  (24h) is longer than the default wall-clock ceiling (4h). The age is
  **floored at 4h** (`worktree.MinPruneAge`, the default ceiling) whatever
  `--older-than` / `FLEET_WORKTREE_PRUNE_AGE` says — a smaller value is raised
  with a warning, never honoured. Aged candidates are then cross-checked
  against `git worktree list --porcelain`: a worktree git reports as
  **locked** is kept, one git knows is removed through git, and only a
  directory git does not list is deleted directly. A box whose wall-clock
  ceiling is raised above 4h must raise the prune age to match; the floor
  cannot see the override.
- **The persistent-session cap stays soft.** See above; a busy session is never
  evicted, so the live count can exceed the limit transiently.
- **No automatic `--deep` prune.** Named-image removal stays a human decision.
