# HTTP contracts: what is generated

Issue #2050, phase 4. The decision and its reasons are in
[ADR-0109](adr/0109-generated-http-contracts.md). This page covers what is
generated today, how to change it, and what phase 3/4 still has to do.

## Generated today

| File | Generated from | By |
|---|---|---|
| `docs/openapi.yaml` (from the generated-marker line down) | the routes the real orchestrator router registers (`buildOrchestratorMux`, walked with `chi.Walk`) joined with the route catalog `internal/apispec/orchestrator/operations.go` | `internal/apispec.Generate`, run by `TestOpenAPIGenerated` (`cmd/fleet`) with `-update` |
| `docs/openapi.yaml` (above the marker) | `internal/apispec/orchestrator/base.yaml`, copied byte for byte | the same |
| `web/src/api/generated/orchestrator.ts` | `docs/openapi.yaml` | `openapi-typescript` 7.13.0 (`web/tools/openapi-typescript`, `--default-non-nullable false`) |

`make api-contracts` regenerates both files. The web half needs `npm ci` in
`web/` first. From `web/`, `npm run gen:api` regenerates the TypeScript alone.

The generator works out these parts itself, so nobody writes them:

- **The path list and each path's methods.** These come from the router walk.
  Generation fails if a registered route has no catalog entry, or a catalog
  entry names a route the router does not register.
- **Path parameters.** There is one for each `{name}` segment, `in: path`,
  `required: true`. It is a string unless `PathParams` gives a type, and that
  type applies on every route.
- **Reference checks.** Generation fails if a `$ref` points to a schema or
  response the base document does not define, or if a security scheme is not
  defined there.
- **Required fields.** Generation fails if an operation has no `security`, no
  `x-fleet-permission`, no summary or no responses. An operation that should
  be open still has to say so with `apispec.Public()`; leaving security out
  does not make a route public by accident.

These parts are still written by hand, and other tests check them:

- **The catalog entries:** summary, description, security, permission, query
  parameters and bodies.
- **The base document,** including the component schemas. These checks cover
  it:
  - `TestOpenAPISchemaDrift` compares component properties, `required` and
    scalar types with the Go models.
  - `TestOpenAPIPermissionEnforcement` and its siblings check
    `x-fleet-permission`.
  - `TestOpenAPITaskStatusEnum` checks the task-status enum against
    `models.AllTaskStatuses`.
- **Status codes and inline (non-`$ref`) bodies.** These are documentation
  only. Nothing checks them against the handlers.

## Changing a route

1. Register the route in `cmd/fleet/orchestrator_mux.go` as before.
2. Add (or rename, or delete) its entry in
   `internal/apispec/orchestrator/operations.go`, keyed exactly as chi
   registers it (`"POST /tasks/{task_id}/wake"`). A new schema goes in
   `base.yaml` under `components.schemas`, and gets a row in the drift test's
   `schemaModelRegistry`.
3. Run `make api-contracts` and commit `docs/openapi.yaml` and
   `web/src/api/generated/` with the change.

Never edit `docs/openapi.yaml` or `web/src/api/generated/` by hand.
`TestOpenAPIGenerated` (in `make test` and the `go` CI job) and the
`contracts` CI job both fail on any difference.

## The CI gate

The `contracts` job in `ci.yml`, named "HTTP contracts (OpenAPI + web types)",
is one of the jobs `CI gate` needs. It runs `npm ci` in `web/`, then
`make api-contracts`, then fails if `docs/openapi.yaml` or
`web/src/api/generated/` differs from the commit (a new untracked file counts
as a difference). Generation fails before the diff on any route↔catalog
mismatch, so the same job is the route↔spec gate.

The job fetches nothing the `go` and `web` jobs do not already fetch: Go
modules through the module proxy, and npm packages through setup-node's
cache. No tool binary is downloaded. `openapi-typescript` is an ordinary
locked npm devDependency, so Dependabot's `/web` npm entry updates it with
the rest of the tree.

## Web types

`web/src/app/shared/lib/orchestratorApi.ts` declares
`type Schemas = components["schemas"]` and aliases the generated types under
their old names, so nothing that imports them had to change. These are
generated now:

- `MCPChoice`
- `RetryPolicy`: `TaskCreate.retry_policy`
- `TaskStatus`: `Task.status`
- `SLAReport` and `SLAReportTask`
- `UsageReport`, `UsageBucket` and `UsageGroupBy`
- `AdoptionReport`, `AdoptionUser`, `AdoptionSeat`, `AdoptionDay` and
  `AdoptionTotals`
- `BudgetStatus` and `BudgetCreate`
- `CostForecast` and `CostRange`

The imports are type-only, so they add no bytes to the bundle.

## Why openapi-typescript is a workspace of its own

`openapi-typescript` builds its output with the TypeScript 5 compiler API
(`ts.factory`). TypeScript 7, the native compiler `web/` type-checks with,
no longer exports that API. A peer-dependency override cannot fix this,
because npm places a peer next to its parent. So the generator lives in
`web/tools/openapi-typescript`, an npm workspace of `web/` with
`typescript@5.9.3` beside it:

- It shares `web/`'s one lockfile, so `npm ci` and `npm audit` cover it.
- It has devDependencies only and ships nothing.
- `stack.conf` declares it, which stack-check requires of every package.json.

When `openapi-typescript` supports TypeScript 7, the workspace folds back
into `web/package.json`.

## What phase 3/4 still has to do

Phase 3 builds one chi router and one listener. Phase 4 completes on top of
it:

1. **Walk the unified router once.** Each family gets its mount point in
   `Family.Prefix`: orchestrator at `/v1`. Add a test that the browser
   alias mount (`/api/orchestrator`) registers the same route set, then
   delete the orchestrator rows of `internal/webui/routes.go`. The non-1:1
   ones (`/config` → `/api/config`, the query allow-lists) become explicit
   catalog routes or SPA changes (ADR-0109 §2).
2. **Chat (`internal/httpapi`, about 70 routes, `net/http.ServeMux`).** It
   can be walked once its routes are on the chi tree. It needs an
   `internal/apispec/chat` catalog and base document and a `docs/` output,
   either a second document or a second family merged into one. The
   streaming routes need their SSE event shapes described. Their wire
   contract is pinned today by `testdata/contracts/chat-stream`.
3. **webui's own routes** (auth, SSO, theme, brand, uploads): the same,
   once they are on the tree.
4. **Component schemas from Go types.** Generate `components.schemas` by
   reflection over the models the drift test already binds
   (`schemaModelRegistry`). Constraints Go cannot express (`minLength`,
   `enum`, descriptions) go in struct tags or a small per-type override
   table. `base.yaml` then shrinks to info, servers, extensions and
   security schemes.
5. **Security from the Principal middleware.** Once phase 1's typed
   `Principal` middleware takes a declared requirement per route, generate
   `security` and `x-fleet-permission` from that declaration, so the code
   that enforces a requirement also documents it. Today they are written in
   the catalog and checked by the enforcement tests.
6. **Convert the remaining web mirrors.**
   - In `orchestratorApi.ts`: `Task`, `TaskCreate`, `RunIf`,
     `TaskTemplateTask`, `Dataset*`, `PromptLibrary*`, `DashboardStats`,
     `TaskLearnedInstruction`, the log and stream types.
   - Every chat-side type, after step 2.
   - `Task` and `TaskCreate` need their spec schemas finished first. The
     spec marks most `Task` fields optional and lacks about thirty the SPA reads,
     so converting them today would loosen the SPA's types.
7. **Status codes.** Optionally, check documented response codes against
   handler behaviour. Nothing checks them today.
