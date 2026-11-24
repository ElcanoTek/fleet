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
| `SweepExpired` | Temporary conversations past `CONVERSATION_TTL_DAYS` without activity (default 90; no `FLEET_` prefix — it predates the alias chain), plus per-user cap eviction (`CONVERSATION_UNPINNED_CAP`, 500). The policy and the warning people get first: [CHAT-RETENTION.md](CHAT-RETENTION.md) | TTL / cap |
| `PurgeTerminalInputs` | Terminal input-queue rows | `FLEET_INPUT_QUEUE_RETENTION_DAYS` (30) |
| `SweepTurnEvents` | Finished turns' durable SSE ledgers | `FLEET_TURN_EVENT_RETENTION_DAYS` (14) |
| `SweepAttachments` | Attachment files past the conversation TTL that nothing kept uses: an upload a message of a still-existing conversation names, and the Multiplayer private stash, stay (`store.AttachmentKeeper`, [ATTACHMENT-SCOPING.md](ATTACHMENT-SCOPING.md#how-long-an-upload-lives)) | conversation TTL, then use |
| `SweepOrphanWorkspaces` | `<workspace>/<convID>/` dirs with no live conversation row; each candidate is looked up again just before removal, so a chat created during the pass keeps its dir | liveness, not age |
| `CleanupTempFiles` | Orchestrator `temp_uploads` files no kept job names: a job with a pending, scheduled, running, paused or dead-lettered task, a recurring job whose next occurrence is not spawned yet, or a job that finished within the window (`storage.TempUploadKeeper`) | `FLEET_SCRATCH_RETENTION_DAYS` (14 by default), then use |
| `SweepExpiredOAuthFlows` | Abandoned per-user remote-MCP OAuth flow rows | flow expiry |
| `worktree.PruneStale` | Orphaned per-run git worktrees | `FLEET_WORKTREE_PRUNE_AGE` (24h) |

Each store method treats a non-positive TTL as *disabled*, so turning a
retention knob off yields a no-op rather than a surprise deletion.

**A reclaimer with nothing to reclaim is silent.** Most boxes never receive an
orchestrator upload and never enable worktree isolation, so `temp_uploads` and
`.fleet-worktrees` simply do not exist on them. Both sweeps treat an absent
root as *nothing to clean* and log nothing — an hourly ticker turns one
cosmetic log line into 24 a day, and an operator who learns to ignore this
pass stops reading it when it has something real to say. A stat failure that
is **not** absence (a permissions problem, an unreadable `.git`) is still
reported: that one an operator must see.

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

A run that fires while the fleet service is down (stopped, or never started on
a fresh box) skips the podman prune and exits 0: podman's runtime dir
`/run/fleet` exists only while `fleet.service` runs, and the maintenance unit
cannot create it. The journal says so ("podman runtime dir /run/fleet does not
exist ... Skipping image cleanup"), the unit records success, and the next
daily run retries. A prune that runs and fails still fails the unit.

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

### Why shedding never answers 503

`/readyz` is the readiness gate (the Helm chart's readiness probe, and what a
load balancer should watch): a 503 there drains chat traffic off the box.
`/healthz` stays 200 for disk too, since a load balancer may still point at it.
A shedding box is precisely one whose chat is fine — so draining it would
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
| `running`, run ended but its outcome write fails | The runner retries the write while it keeps the lease; a run that ended without its outcome landing is dead-lettered by `RecoverExpiredLeases` rather than run again, unless the outcome was itself a request to run again ([below](#when-a-runs-outcome-cannot-be-written)) | the outage; 10s past a force-cancel |
| `running`, agent hung | Per-run wall-clock deadline | `FLEET_TASK_WALL_TIMEOUT` (4h) |
| `running`, lease stolen | `errTaskLeaseLost` cancels the stale run | immediate |
| `paused_awaiting_input` | `ExpirePausedTasks` fails it terminally | `FLEET_PAUSED_TASK_EXPIRY_MINUTES` (off by default) |
| `paused_awaiting_wake`, unreachable | `ExpireStrandedWakeTasks` fails it terminally | 24h past the deadline |
| Interactive turn hung | Per-turn context deadline | `FLEET_TURN_TIMEOUT_SECONDS` (default 14400) |

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

A dead-lettered occurrence of a recurring task does the same (ADR-0070): the
next run is queued unless the previous occurrence was also dead-lettered, in
which case the chain parks until an operator replays. Cancel still ends the
chain. See [`DEAD-LETTER-RECURRENCE.md`](DEAD-LETTER-RECURRENCE.md).

There is no disable knob. An unreachable parked row is a broken row, not an
operator policy choice.

### When a run's outcome cannot be written

A scheduled run ends in one lease-fenced write: its success or error, its
retry requeue, its dead-letter, or its ask/wake park. That write used to be a
single attempt. A Postgres restart, failover or connection reset at that
moment left the row `running`; the worker returned, its lease renewals
stopped, and five minutes later `RecoverExpiredLeases` re-queued the task, so
the whole run happened again, emails and MCP writes included, as
`[retry] attempt 2`. With `max_retries=0` it was dead-lettered as a crash loop
instead, with no notification. A wall-clock timeout or a self-audit abort,
which are never retried, re-ran the same way.

Now every outcome write goes through one helper in the runner
(`recordOutcome`, `internal/runner/outcome.go`):

- **It retries while the run keeps its lease.** A failure the database
  reports as transient (a restart or shutdown, a dropped or refused
  connection, a timeout, too many connections, a serialization failure or
  deadlock) is retried with backoff: 250 ms, doubling to 15 s between
  attempts, each attempt bounded at 15 s. The run stays in the pool while it
  retries, so its lease renewal keeps going. The pool slot stays taken
  meanwhile, which is also what keeps `fleet update` waiting.
- **Its own server's lease recovery leaves it alone.** An outage can outlast
  the lease, because the renewals fail with it. The scheduler's lease
  recovery skips every row leased by a run this server still holds, from just
  before the claim commits until the run's outcome has landed or been given
  up (`Pool.HeldLeaseTokens`, registered with `Storage.SetLeaseHolder`). When
  the database answers again, the sweep may run first; it leaves the row, and
  the run's next attempt lands. Renewals and outcome writes are fenced on the
  lease owner, not on the expiry time, so an expired lease that nobody
  recovered still accepts them. Rows leased by other servers are recovered as
  before.
- **It is idempotent.** Each outcome write records the run's claim token
  (`sched.tasks.finished_run_id`, migration 0213) in the same statement that
  releases the lease. When a write is refused by the lease fence, or its
  COMMIT never answers (every transactional outcome write reports that case
  as `ErrTaskCommitOutcomeUnknown`), the runner reads the row back. Its own token, a
  released lease and the status it was writing mean the write had already
  landed, so the run's notification and email reply fire once.
- **A refusal no retry can fix is not retried.** For example, a constraint
  violation or a structured-output contract error. The run records a fallback
  instead: a dead-letter or a retry requeue falls back to a terminal error, as
  before, and a structured task's success takes the structured-output
  persistence class. A free-form success becomes a terminal `error` whose
  message says the success could not be recorded and the run was not run
  again. Before, it stayed `running` until lease recovery ran it again.
- **Shutdown bounds it.** Retries continue through the drain grace
  (`FLEET_SHUTDOWN_GRACE_SECONDS`). When the pool force-cancels its runs, every
  write still pending, including the interrupted runs' own outcomes, gets one
  shared 10 s window, then the runner gives up. That fits inside the 30 s the
  deploy unit allows past the grace.
- **A run that ended is not run again by recovery.** As a run ends, before
  its outcome is written, the runner stamps `finished_run_id` under its
  lease. If the outcome never lands, the row stays `running` with that
  stamp: the database stayed unreachable until the server stopped, the
  server was killed between the stamp and the write, or the outcome and its
  fallback were both refused. A refusal does not wait for a stop: the worker
  returns, and the lease runs out about five minutes later with the server
  still running. Once the lease has expired, `RecoverExpiredLeases`
  dead-letters the row, whatever its retry budget, with the reason "outcome
  not recorded: the run finished, but its result could not be saved before
  its lease expired…". It counts as
  `fleet_dead_letter_queued_total{reason="outcome_unrecorded"}`. Read the run
  log, then replay it (`fleet sched dlq replay`) if it should run again. A
  recurring task's next occurrence is spawned by the recurrence sweep, as for
  every recovery dead-letter.
- **A run that asked to run again is not stamped.** The retry requeue and the
  connector-outage re-run send the task back to run again, so the runner
  does not stamp the run finished before writing them, and first clears a
  stamp an earlier write of the same run left (a structured success that was
  refused and is retried through `retry_on`). If such a write never lands,
  recovery re-queues the task like a crash (attempt + 1), which is what the
  run asked for. If clearing that earlier stamp fails too, the stamp stays
  and recovery dead-letters the run instead.

A run the process died in the middle of never stamped anything, so it is
recovered as before: re-queued while its retry budget lasts (and run again as
attempt N), dead-lettered by the crash-loop guard past it.

What this does **not** cover:

- **The stamp is a write too.** If the database was already unreachable when
  the run ended and stays so until the server stops, nothing records that the
  run ended, and recovery re-queues it like a crash.
- **Another server's lease recovery.** It does not know which runs this
  server holds. If an outage outlasts the lease and another fleet server on
  the same database recovers the row first, the stamp makes it a dead-letter
  rather than a second run, but the outcome the runner held is not the one
  recorded.
- **A run that hangs while its renewals keep failing** stays `running` until
  it ends or its server restarts, since its own server no longer recovers it.
  By default the wall-clock timeout (`FLEET_TASK_WALL_TIMEOUT`, 4h) bounds the
  run; `FLEET_TASK_WALL_TIMEOUT=0` removes that bound.
- **No notification for a recovery dead-letter**, the same gap as the expiry
  sweeps above: the notifier lives in the runner.
- **Transcripts and metadata are still single attempts.** The run log
  (`AddLog`), the published-artifact manifest, the workspace path and clearing
  answered questions or wake state are best effort. A failure there loses that
  record, but it never runs the task again.
- **Chat turns and patrols were never affected.** A chat turn stranded by a
  crash is recovered as an interrupted turn, never re-run. A patrol fire whose
  settle fails is delivered again under the same fire id, which does not
  queue a second turn.

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
| `FLEET_RUN_LOG_RETENTION_DAYS` | `90` | Age before terminal task runs are pruned. Their recorded spend stays in usage reports and budgets. `0` disables. |
| `CONVERSATION_TTL_DAYS` | `90` | Days without activity before a temporary chat is deleted. |
| `CONVERSATION_UNPINNED_CAP` | `500` | Most temporary chats one person keeps; the least recently active go first. |
| `FLEET_CONVERSATION_EXPIRY_WARNING_DAYS` | `14` | How long before deletion a chat is flagged in the interface. `0` turns the warning off. |
| `FLEET_SCRATCH_RETENTION_DAYS` | `14` | Age before orchestrator upload files (`temp_uploads`) are removed, once no job that can still run names them and the job naming them finished at least that long ago. |
| `FLEET_INPUT_QUEUE_RETENTION_DAYS` | `30` | Age before terminal input-queue rows are purged. `0` disables. |
| `FLEET_TURN_EVENT_RETENTION_DAYS` | `14` | Age before finished turns' SSE ledgers are swept. `0` disables. |
| `FLEET_PAUSED_TASK_EXPIRY_MINUTES` | `0` (off) | Fail a task awaiting input longer than this. |
| `FLEET_TASK_WALL_TIMEOUT` | `4h` | Per-run wall-clock ceiling. `0` disables. |
| `FLEET_PYTHON_REPL_MAX` | `32` | Persistent sandbox session cap (soft). `0` = unbounded. |
| `FLEET_PYTHON_REPL_IDLE_TTL` | `2h` | Idle time before a persistent sandbox is reaped. |

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
  against the repository's records in `.git/worktrees`: a worktree a record
  marks as **locked** is kept, one a record names is removed together with
  that record, and a directory no record names is deleted on its own. A box
  whose wall-clock ceiling is raised above 4h must raise the prune age to
  match; the floor cannot see the override. The sweep runs no git at all
  (ADR-0118): it reads and deletes through no-follow `os.Root` handles on
  `.fleet-worktrees` and `.git/worktrees` (`tools.OpenWorkspaceDir`), skips
  entries that are symlinks, and considers only records whose `gitdir` names
  a directory under `.fleet-worktrees`. No checkout is removed before the
  record its `.git` file names has been read: a lock there keeps it (the
  `initializing` lock fleet holds only while it writes a record counts for
  nothing on a worktree this old), and a `.git` file or record that cannot
  be read keeps it too; a checkout with no `.git` file, or whose record
  provably does not exist, is an orphan. A record holding nothing but an
  `initializing` lock older than the prune age (a server stopped while
  writing it) is removed. Nothing is removed unless the root's `.git` is the
  real directory recorded when fleet started and passes its check; a `.git`
  that appeared or changed since is reported once per process and left alone
  until a restart. The directories the server creates at start may still be
  missing, so `fleet worktree prune` before an upgraded server's first start
  still honours locks. `fleet worktree prune` and `fleet worktree list`, separate
  processes, record the root as they find it when the command starts; `list`
  runs no git either and prints what a run could write (a lock reason) with
  control characters replaced. Every read of a record file Lstats it, opens it
  without blocking and checks it is the same regular file (a link or a FIFO
  there is skipped). A `.fleet-worktrees` that is a symlink or not a directory
  is reported as an error on every pass until it is removed.
- **The persistent-session cap stays soft.** See above; a busy session is never
  evicted, so the live count can exceed the limit transiently.
- **No automatic `--deep` prune.** Named-image removal stays a human decision.
