# ADR-0045: Remove node-name scopes; a principal's authority is its permission set

- **Status:** Accepted
- **Date:** 2026-08-15
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0011](0011-remove-worker-node-registry.md) (its retention of
  `allowed_node_patterns` / `users.scopes`)

## Context

fleet inherited a second authorization axis from moc, alongside permissions: a
list of **node-name glob patterns** on every principal (`users.scopes` for user
accounts, `api_keys.allowed_node_patterns` for API keys). A task was routed to a
named worker node, and a principal could only see and act on tasks whose node
matched one of its patterns.

[ADR-0011](0011-remove-worker-node-registry.md) removed the node registry. It
kept the glob mechanism deliberately, on the theory that it was "the shared
task-visibility scope concept, cosmetically node-named" and worth retaining "for
forward compatibility of API-key scoping."

That theory did not survive contact with the code. With no node to match
against, the scope surface degenerated into a no-op that still *looks* like an
authorization boundary:

- **The visibility predicate was a constant.** `taskVisibleToScopes` returned
  `true` on every path — its own doc comment said so ("The result is therefore
  always true: every task is visible to every principal that reaches this
  check"). Seven call sites in `handlers.go` wrote `403 Task not within allowed
  scopes` on the branch that could never be taken; three more (pause, upcoming,
  learned-instructions) did the same.
- **The list filter discarded its input.** `TaskFilter.VisibleToScopes` was
  consumed by a literal `_ = filter.VisibleToScopes` in `GetTasksFiltered`. Its
  only live effect was to force the query down the filtered code path instead
  of the paginated one, for identical results.
- **The scoped stats query was the unscoped one.** `GetDashboardStatsForUser`
  built `WHERE (created_by = $9 OR TRUE)` — a hand-rolled duplicate of
  `GetDashboardStats` reachable only through a cache key that varied by a
  pattern list that changed nothing.
- **The one real matcher was unreachable.** `APIKey.CanTargetNode` (and the
  `storage.MatchGlob` helper under it) ran only when `ValidateKey` was passed a
  target node name. Every production call site passed `nil`; nothing has passed
  a node name since ADR-0011.
- **Operators could still configure it.** `fleet-admin sched user add --scopes`,
  the `allowed_node_patterns` field on `POST /keys`, and the legacy-import
  `scopes` array all accepted patterns, persisted them, and echoed them back —
  a documented knob whose value was never read.

[ADR-0043](0043-per-task-run-log-scoping.md) had already hit this from the other
side: a run-log scope check added after a security audit turned out to be a
no-op for exactly this reason, and that ADR explicitly rejected "make
`taskVisibleToScopes` mean something again" as a fix. This ADR finishes the
thought by deleting the machinery instead of routing around it a third time.

"Forward compatibility" for a concept with no semantics is indistinguishable
from dead code, and this dead code advertised a security property fleet does not
have. That is the failure mode `AGENTS.md`'s **honesty** invariant exists to
prevent, and it is worse here than ordinary rot: an operator reading the API
reference could reasonably mint a "scoped" key believing it narrows what that
key can see.

## Decision

**Remove node-name scopes entirely.** A principal's authority is its permission
set, plus the typed-key gates that do enforce something real (trigger slugs,
budgets, the priority ceiling), plus creator ownership where a resource is
private.

1. `principal.scopes()`, `taskVisibleToUser`, and `taskVisibleToScopes` are
   deleted along with all ten handler call sites and their unreachable 403s.
2. `TaskFilter.VisibleToScopes` / `VisibleToUserID` are deleted;
   `GetTasksFiltered` no longer has a discarded field. `ListTasks` stops forcing
   the filtered path for a scoped principal.
3. `GetDashboardStatsForUser` is deleted from both `db` and `storage`; the
   handler resolves stats through `GetDashboardStats` under a single cache key.
4. `APIKey.AllowedNodePatterns`, `APIKey.CanTargetNode`, the `targetNodeName`
   parameter of `Manager.ValidateKey`, the `node_access_denied` audit action,
   and the `allowed_node_patterns` create-parameter of `CreateKey` /
   `CreateTypedKey` are deleted. `storage.MatchGlob` — whose only caller was
   `CanTargetNode` — goes with them.
5. `User.Scopes`, `UserCreate.Scopes`, `UserResponse.Scopes`,
   `APIKeyCreate.AllowedNodePatterns`, and `APIKeyResponse.AllowedNodePatterns`
   leave the models and `docs/openapi.yaml`. Migration
   `062_drop_user_scopes` drops the column.
6. `fleet-admin sched user add --scopes` is removed. The comma-list parser it
   shared with `--trigger-slugs` survives under an honest name (`parseCSVList`),
   since trigger-slug scoping is real.

**No authorization boundary is weakened.** Every deleted check was proven
constant by inspection — `taskVisibleToScopes` returned `true`, the filter field
was discarded, and `CanTargetNode` was unreachable. The checks that do enforce
something (permissions, typed-key gates, `ownsTask`, the creator-scoped
workspace and run-log gates of [ADR-0043](0043-per-task-run-log-scoping.md)) are
untouched. This ADR amends ADR-0011's retention consequence only; ADR-0011's
decision to remove the node registry stands.

## Enforcement

- `cmd/fleet/openapi_drift_test.go` (`TestOpenAPISchemaDrift`) fails the build
  if `allowed_node_patterns` reappears in the spec without a backing model
  field.
- `internal/sched/db/migrations/062_drop_user_scopes.up.sql` drops the column
  under a reviewed `migration-lint: allow-dangerous` directive; no code reads it
  on either side of the change.
- The `x/tools` `deadcode` analyzer over `./...` reports no unreachable function
  left behind by this removal.

## Consequences

- `POST /keys` and `POST /users` silently ignore an `allowed_node_patterns` /
  `scopes` field an old client still sends, instead of storing it and reporting
  it back. Nothing changes about what that client can do, because the stored
  value never narrowed anything. The API responses no longer carry the fields.
- A legacy import dump that carries `sched.users[].scopes` still imports; the
  array is ignored rather than preserved. Documented in
  [`../LEGACY-IMPORT.md`](../LEGACY-IMPORT.md).
- Dropping `users.scopes` is irreversible for the stored patterns; the down
  migration restores the column shape only, matching the other destructive down
  migrations here. No value was ever read, so nothing is lost.
- If per-principal task partitioning is ever wanted, it starts from a designed
  predicate over something tasks actually carry — tags, creator, or project —
  not from a resurrected node-name glob. This ADR is the one to supersede.

## Alternatives rejected

- **Finish it instead: re-point the globs at tags or titles.** That is not
  finishing this feature, it is designing a different one under its name.
  Multi-tenant task partitioning is a real feature with real semantics
  (inheritance, defaults, what an unscoped principal sees, how a task acquires
  a scope) and deserves its own issue and ADR — not a pattern list that
  historically matched hostnames.
- **Keep the columns, delete only the checks.** Leaves two configurable,
  API-visible fields that persist operator input and do nothing — the same
  honesty problem in a smaller box, and the exact "vestigial retention has
  negative value" trap ADR-0011 named while falling into it here.
- **Keep it dormant behind a flag.** There is nothing to flag on: no code path
  exists that a scope pattern could narrow.
