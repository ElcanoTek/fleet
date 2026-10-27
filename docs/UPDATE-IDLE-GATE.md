# Update and Service Lifecycle Idle Gate

## What shipped

`fleet update` (`scripts/update.sh`), `fleet restart`, and `fleet stop` (`internal/admincli/service.go`) now inspect the running deployment's Postgres databases (`sched.tasks`, `chat.turns`, and `chat.approvals`) before mutating or restarting `fleet.service`, and quiesce new work admission while draining.

### 1. Active-work detection (`scripts/lib/idle.sh` and `internal/admincli/idle.go`)

- **Scheduled tasks (`sched.tasks`):** Reports tasks with `status IN ('running', 'leased', 'assigned')` that either hold an unexpired lease (`lease_expires_at >= now()`) or were started by the current `fleet.service` process (`COALESCE(started_at, created_at) >= to_timestamp(ExecMainStartTimestamp)`), so tasks that continue across a transient database hiccup remain protected while crash-stranded rows from a prior process are ignored once their lease expires.
- **Table-agent dataset runs (`sched.datasets`):** Reports datasets with `status = 'running'` started in the current service instance (`COALESCE(updated_at, created_at) >= to_timestamp(ExecMainStartTimestamp)`), and `datasets.Runner.Start` refuses new dataset runs with `503 Service Unavailable` while draining.
- **Chat turns (`chat.turns`):** Reports turns with `status = 'running' AND finished_at IS NULL` started at or after the current service instance's `ExecMainStartTimestamp` (with no upper age cutoff, so long research or tool-heavy turns are never aged out while the process that started them is still active).
- **Detached approval executions (`chat.approvals`):** When the `is_err` column from chat migration 061 is present, reports approved tool executions still in flight (`status = 'approved' AND is_err IS NULL AND result_text LIKE 'Approved%executing%'`) started in the current service instance within the 2-hour window. Pre-061 schemas without `is_err` skip the query cleanly so upgrades from older releases never fail closed before migrations run on service start.
- **Stopped-service recovery:** When `systemctl` reports the target unit is installed and not `active`, `ActiveWork` / `fleet_active_work_summary` treats any database rows as crash-stranded so `fleet restart` and `fleet update` can start the service and let boot recovery (`RecoverExpiredLeases` / `RecoverStrandedTurns`) clean them up.
- **Fail-closed when active:** When `systemctl` reports the target unit is `active`, any database query failure during `WaitIdle` / `fleet_active_work_summary` is treated as an active-work blocker rather than silently assuming idle.

### 2. Pre-restart admission drain (`internal/httpapi/server.go`, `cmd/fleet/main.go`)

- While `fleet update`, `fleet restart`, or `fleet stop` is waiting for idle (or transitioning from the final idle check to `systemctl restart`/`stop`), it writes and refreshes the drain sentinel file (`<FLEET_ENV_FILE>.update.drain`, overrideable via `FLEET_UPDATE_DRAIN_FILE` or `FLEET_DRAIN_FILE`).
- `httpapi.UpdateDrainActive()` and `(*httpapi.Server).IsDraining()` observe this sentinel (with a 15-minute staleness bound refreshed on every poll iteration during long `--wait-timeout` runs):
  - `POST /v1/chat`, webhook turn admission, queued turn handoff (`input_queue.go`), and approval execution/resumption return `503 server_draining` while draining.
  - `diskAdmissionGate` in `cmd/fleet/main.go` returns ` ErrAdmissionPaused` to `runner.Pool` while `httpapi.UpdateDrainActive()` is true, pausing new scheduled-task claims without cancelling in-flight runs.
- The drain sentinel is removed automatically on exit (`trap ... EXIT` / `defer ClearUpdateDrain()`) and immediately after `systemctl restart` or `systemctl stop` completes.

### 3. Operator CLI & status visibility

- `fleet update`, `fleet restart`, and `fleet stop` accept:
  - `--wait` (`FLEET_UPDATE_WAIT=1`) and `--wait-timeout <duration/seconds>` (`FLEET_UPDATE_WAIT_SECONDS=600`, default 10 minutes) to drain and wait for in-flight work to finish.
  - `--force` (`FLEET_UPDATE_FORCE=1`) to bypass the active-work check when an immediate restart is required.
- `fleet status` and `fleet diagnose` surface an `active work` check showing whether the deployment is `idle` or listing in-flight tasks, turns, and approval executions.

## Deviations and deferred work

- **No cross-restart chat turn checkpointing:** In-flight LLM chat streams and container tool calls remain bound to the running `fleet.service` process; rather than serializing mid-turn container state across process restarts, updates block or drain until active turns complete.
- **First upgrade from a pre-sentinel daemon:** Pre-restart admission quiescing via the `.update.drain` sentinel file and `/healthz` in-memory counters requires the currently running `fleet.service` binary to include this gate. During the one-time upgrade from an older release that predates the sentinel, `fleet update` enforces the Postgres active-work checks (`sched.tasks`, `chat.turns`, `chat.approvals`) before building, installing, and restarting (backed by the old daemon's SIGTERM graceful shutdown drain), and full pre-restart admission quiescing is active on all subsequent updates and restarts.
