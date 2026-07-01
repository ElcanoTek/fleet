# Live run visibility & take-over

Watch what an agent run is doing, tool by tool, as it happens — and stop it —
for both interactive chat and scheduled tasks (#508).

## Interactive chat (pre-existing, for reference)

- **Live activity**: tool-call pills stream into the transcript as each call
  starts/finishes ("Show details" toggle); the thinking indicator names the
  in-flight tool.
- **Stop**: the composer's Stop button cancels the turn at the governed loop's
  next checkpoint; the partial transcript persists and the turn records
  `Cancelled`.
- **Take-over**: Stop, then send — a new message on the same conversation
  cancels and replaces the in-flight turn server-side.

## Scheduled tasks (what #508 added)

- **Live activity view**: opening a *running* task in the Operations Center
  attaches to `GET /tasks/{id}/stream` (SSE — `tool_call` / `tool_result` /
  `agent_message` / `status` frames from the run's observer stream) and renders
  the activity chronologically, auto-following. Finished tasks keep the
  existing persisted-log view; the stream replay's terminal frame now reports
  the task's REAL outcome (previously hardcoded `succeeded`).
- **Stop with attribution**: `DELETE /tasks/{id}` now (1) flips the row to
  `cancelled` with `"stopped by <principal>"` recorded on the terminal result,
  and (2) interrupts the live run in-process: the governed loop halts at its
  next checkpoint, an in-flight sandbox exec is killed via its context, the
  sandbox/MCP client are released by the run's existing defers, and the
  partial transcript persists (the log write is lease-free). A stopped task is
  **not** retried, dead-lettered, notified, or error-analyzed — a deliberate
  stop is not a failure. The live stream emits a terminal
  `{"status":"stopped","stopped_by":…}` frame. The Stop button appears on the
  live view for admins (the server enforces the permission).
- **Classification fix**: a force-cancelled run returns a nil error with a
  partial session; the runner previously mislabeled that as `success`.
  Interruption is now keyed on the task context, so shutdown-grace kills
  record as interrupted errors.

## Honest scope (deferred)

- **Redirect/steer a running scheduled task** — per the issue triage,
  redirect/resume comes only after stop is proven. The mapped injection seams
  (enforcement rounds / PrepareStep) are documented in the issue.
- **Stop permission** stays admin-only (`PermissionCancelTask`); a client
  stopping its *own* tasks needs the ownership model #190 deferred.
- Chat's send-while-running take-over stays disabled in the UI (Stop → send
  covers it); `reasoning.*` frames remain deliberately absent from the task
  stream.
