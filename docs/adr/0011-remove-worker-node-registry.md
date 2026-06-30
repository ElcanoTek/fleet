# ADR-0011: Remove the worker-node registry; the in-process worker is the only runner

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers

## Context

fleet inherited a worker-node registry from its multi-runner predecessor (moc):
a `nodes` table, `POST /register` gated by a `REGISTRATION_TOKEN`, per-node API
keys, `GET /nodes` + `DELETE /nodes/{id}`, a heartbeat/lease/report protocol
(`/nodes/heartbeat`, `/tasks/pending`, `/status`, `POST /logs`), `view_nodes`
permission, node-name scope matching, and "Total / Active Agents" dashboard
counters fed by node-status SQL.

None of it is live on the single-box architecture (ADR-0004). The in-process
worker pool (`internal/runner`) claims work directly through
`ClaimNextPendingTask` (FOR UPDATE SKIP LOCKED) under a lease keyed on a
synthetic `lease_owner` UUID and writes status straight to storage — it never
registers a node, never calls the HTTP lease/report endpoints, and never sets
`tasks.assigned_node_id` (NULL for every row since per-task node routing was
dropped in migration 014). No foreign key references `nodes`. The result: the
"Registered Agents" table is always empty, the agent counters are always zero,
and a stack of unmaintained auth surface (`NodeAuthMiddleware`,
`RegistrationAuthMiddleware`, the `REGISTRATION_TOKEN`) implies a capability that
does not exist. The Operations Center issues #459 (remove the dead "Registered
Agents" area) and #458 (auth bugs in that view) made the cost of keeping it
concrete.

## Decision

We **remove the worker-node registry entirely** rather than leave it vestigial:

- Drop the `nodes` table and the `tasks.assigned_node_id` column (migration
  `042_drop_nodes`), and delete the `Node` model, `NodeStatus`, `NodeHeartbeat`,
  `NodeRegistration`, `DeleteNodeResponse`, the `view_nodes` permission, and the
  node-count fields of `DashboardStats` / the health-summary `WorkerStats`.
- Delete the node HTTP surface (`/register`, `/nodes`, `/nodes/{id}`,
  `/nodes/heartbeat`, `/tasks/pending`, `/status`, `POST /logs`), the
  `NodeAuthMiddleware` / `RegistrationAuthMiddleware` / registration rate
  limiter, the `REGISTRATION_TOKEN` config, and all node storage/db methods.
- Delete the now-unreachable node-freeing branches in the lease lifecycle
  (`if task.AssignedNodeID != nil { … free the node … }`); the live ownership
  check is purely `lease_owner`-based.
- Collapse `GET /files/{filename}` (formerly node-key authenticated) to
  admin-only — the in-process worker reads inputs directly from the data dir.
- Remove the "Registered Agents" table and the "Total / Active Agents" stat
  cards from the web Operations Center.

This does **not** add or weaken a security invariant; it is consistent with
ADR-0004 (single box, vertical scale only — "multi-node scheduling is out of
scope") and shrinks the authenticated attack surface. Horizontal scale, if ever
needed, is achieved by running independent fleets per team/department, not by
re-introducing a node registry inside one fleet.

## Enforcement

- `cmd/fleet/openapi_drift_test.go` (`TestOpenAPIRouteParity` /
  `TestOpenAPISchemaDrift`) fails the build if `docs/openapi.yaml` ever
  re-introduces a node path or schema that the router/models don't define.
- `internal/sched/db/migrations/042_drop_nodes.up.sql` drops the table + column;
  there is no code left that reads or writes either.
- The sched storage/db test suites lease through the `leaseTaskToOwner` test
  substrate (a synthetic `lease_owner`, no node row), so the crash-recovery path
  is exercised without the deleted registry.

## Consequences

- The legacy multi-runner HTTP protocol is gone; an external process that spoke
  it (none ship in fleet) would now 404. On a single-box deploy the binary and
  migration land together, so there is no live client to break.
- Dropping `nodes` and `tasks.assigned_node_id` is irreversible data loss; the
  down migration restores the schema only (mirroring the other destructive down
  migrations). Both objects were unused, so no live data is lost.
- The scoped-API-key glob mechanism (`allowed_node_patterns`) is retained — it
  is the shared task-visibility scope concept, cosmetically node-named, not node
  routing. It no longer narrows anything by node but stays for forward
  compatibility of API-key scoping.

## Alternatives considered

- **Keep the table + model, remove only the UI.** Rejected: leaves dead auth
  middleware and a live-looking `REGISTRATION_TOKEN` — an unmaintained surface
  and a credential that implies a capability fleet does not have. Vestigial
  retention has negative value here.
- **Keep the lease/report endpoints "for protocol compatibility."** Rejected:
  they have no in-process or external client and depend on a registered node
  existing, so they die with the table regardless.
