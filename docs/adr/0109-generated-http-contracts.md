# ADR-0109: Generated HTTP contracts

- **Status:** Accepted
- **Date:** 2026-11-08
- **Deciders:** fleet maintainers
- **Issue:** #2050 ("One HTTP layer"), phase 4
- **Design note:** [docs/HTTP-CONTRACTS.md](../HTTP-CONTRACTS.md)

## Context

`docs/openapi.yaml` (about 3.3k lines) describes the orchestrator and was
written by hand. Tests in `cmd/fleet` kept parts of it honest: route parity
against the real chi router, component schemas against their Go models by
reflection, and `x-fleet-permission` against real enforcement. Everything else
was trusted. The web app mirrored the same shapes in hand-written TypeScript
(`web/src/app/shared/lib/orchestratorApi.ts`), several of them marked "mirrors
models.X".

Converting those mirrors to the spec showed what that trust cost. Each of the
following was wrong in the committed spec, and no test caught it:

- `Task.status` listed six statuses. Two of them (`completed`, `failed`) are
  not statuses a task can have, and six real ones were missing (`leased`,
  `success`, `error`, `dead_lettered`, `paused_awaiting_input`,
  `paused_awaiting_wake`). The schema-drift gate checks property names and
  types, not enum values.
- `UsageBucket` did not mention `uncached_tokens`, `cache_hit_rate_pct` or
  `chat_prompt_tokens`. The server sends all three and the web UI reads them.
  The drift gate only printed a warning.
- `CostForecast` and `CostRange` marked every field optional, but the Go
  model always sends them. Only the hand-written TypeScript type had them
  right.

Issue #2050 asks us to generate the contract and the client types from one
source, and to add a CI gate that fails on drift. It leaves three questions
for this decision.

## Decision

### 1. Code-first: the router is the source of truth for routes

The orchestrator's contract is generated from the router plus a route
catalog written in Go (`internal/apispec`):

- **Routes** come from walking the real router (`chi.Walk`). The generator
  fails if a registered route has no catalog entry, or if a catalog entry
  names a route the router does not register.
- **Path parameters** come from the `{name}` segments in each pattern. They
  are typed once per name (`Family.PathParams`), so `datasetID` is a UUID on
  every route that uses it.
- **Everything the router cannot know** is written in the catalog, keyed
  `"METHOD /pattern"`: summary, description, security, `x-fleet-permission`,
  query parameters, request body and responses. Every `$ref` and every
  security scheme must exist in the base document.
- **Document-level parts** (info, servers, the `x-fleet-*` extensions,
  `components`) stay in a hand-maintained base document
  (`internal/apispec/orchestrator/base.yaml`). The existing reflection,
  enforcement and enum tests keep it honest.

`docs/openapi.yaml` is now an output: the base document, byte for byte,
followed by the generated `paths`. `web/src/api/generated/orchestrator.ts` is
generated from it by `openapi-typescript`, pinned as a devDependency.
`make api-contracts` regenerates both files. The CI job `contracts` runs the
same command and fails on any diff.

**Why not spec-first.** Spec-first means generating Go server stubs from a
hand-written spec (for example oapi-codegen's chi server) and making the
handlers implement them. In this codebase that means:

- The spec would decide which routes exist. Today the router decides, and
  phase 3 is about to restructure it.
- The 97 operations sit in two auth groups plus about twenty routes with
  per-route rate limiting or auth inside the handler. A generated registrar
  applies one middleware chain to everything, so those differences would have
  to be rebuilt on top of it.
- Handlers would change signature to take typed parameters. That is 34 reads
  of `chi.URLParam` in 16 handler files, and 34 `URL.Query()` call sites,
  across a package that the phase 1/2 work is changing at the same time.
- Server stubs would add a new runtime Go dependency. The dependency policy
  (AGENTS.md) asks a dependency to earn its place, and this one would mostly
  add an interface on top of handlers that already work.

Code-first touches no handler. It costs one catalog entry per route.

**Boilerplate, measured on real handlers.** These are line counts for the
hand-written YAML before this change and for the Go catalog entry after it.
In both cases the handler itself is unchanged.

| Operation | Hand YAML (operation + the path item's `parameters` block, shared by its methods) | Go catalog entry |
|---|---|---|
| `GET /keys/{key_id}` | 7 + 5 | 9 |
| `GET /tasks/{task_id}` | 11 + 5 | 9 |
| `POST /tasks/{task_id}/tags` | 20 + 5 | 14 |
| `GET /admin/budgets` | 26 | 24 |
| `PUT /tasks/{task_id}/owner` | 45 + 5 | 45 |
| `DELETE /tasks/{task_id}/permanent` | 30 + 5 | 28 |
| All 97 operations | 2,439 | 1,952 |

Spec-first would cost a signature change plus a parameter-binding change in
every handler that takes a path or query parameter, on top of the same
per-operation YAML. That estimate is counted from the call sites above. No
generator was run against this tree to measure it.

### 2. Backwards compatibility while paths are unified: aliases by mounting, one versioned prefix

External API-key clients reach the orchestrator at `/v1/*`, plus the paths
fleet treats as unversioned forever: `/api-info`,
`/.well-known/agent-card.json`, `/a2a`, `/triggers/*`. Caddy exposes only
these (ADR-0053). `docs/openapi.yaml` names `/v1` as `servers[0]`. The
browser reaches the same handlers through `/api/orchestrator/*`. That path is
same-origin and private to the SPA, and `internal/webui` translates it today.
The bare paths (`/tasks`, …) are loopback-only and already carry
`Deprecation` and `Link` headers (`internal/apiversion`).

Phase 3 keeps `/v1` as the public contract and mounts the same orchestrator
sub-router under both `/v1` and `/api/orchestrator` on the one chi tree. The
browser path becomes an alias of the same routes, not a second table:

- **No redirects.** A 307/308 on a `POST` depends on the client following it
  with the body and the `X-API-Key` header intact. curl does not follow
  redirects by default, and webhook senders generally do not follow them on
  delivery. A redirect would break exactly the external clients this rule is
  meant to protect.
- **No new version prefix.** Nothing under `/v1` changes shape, and
  `docs/api-versioning.md` reserves a new major version for breaking changes.
- **A few browser routes are not a plain prefix strip.** Examples:
  `/api/orchestrator/config` → `/api/config`, and the query-parameter
  allow-lists. Each becomes either an explicit browser-only route described
  in the catalog, or an SPA call moved to the canonical path, decided route
  by route.
- **The generator documents the `/v1` mount** (`Family.Prefix`). A test
  asserts that the browser mount registers the same route set, so the alias
  cannot drift.

### 3. Family by family, no flag day

`apispec.Generate` takes one `Family` (catalog, base document, mount prefix)
at a time. Families move independently:

- **orchestrator: generated now.** Its router is chi, so it can be walked.
- **chat (`internal/httpapi`, about 70 routes):** `net/http.ServeMux` has no
  walk API, so chat's contract starts when phase 3 moves those routes onto
  the chi tree. A recording wrapper around `HandleFunc` would work earlier,
  but it would be thrown away weeks later.
- **webui's own routes** (login, SSO, theme, uploads): same as chat, once
  they are on the chi tree.

Each family gets its own catalog package. The web app switches to generated
types one module at a time, and the hand-written types that remain are
visible in review.

## Consequences

- Adding, renaming or removing an orchestrator route without updating the
  catalog fails `go test ./cmd/fleet` (`TestOpenAPIGenerated`) and the
  `contracts` CI job. A hand edit to `docs/openapi.yaml` fails the same
  checks. The edit belongs in `internal/apispec/orchestrator`.
- `docs/openapi.yaml` is re-serialised from typed values, so its `paths`
  section changed format once. The document has the same meaning as before,
  apart from the three fixes in Context, which this PR makes on purpose.
  The only other differences are two equivalences OpenAPI defines:
  - `DELETE /admin/budgets/{budget_id}` declared its path parameter on the
    operation; it now sits on the path item.
  - Explicit `required: false` values (the default) are omitted.
- `openapi-typescript` drives the TypeScript 5 compiler API, which
  TypeScript 7 (web's compiler) no longer ships. It therefore lives in a
  build-time npm workspace, `web/tools/openapi-typescript`, with
  `typescript@5` beside it. That workspace has one lockfile with web/, no
  runtime dependencies, and Dependabot's existing `/web` npm entry covers it.
  `stack.conf` declares it, as stack-check requires for every package.json.
- The SPA imports generated types for the orchestrator report, budget,
  forecast, MCP-choice, retry-policy and task-status shapes. These are
  type-only imports and add nothing to the bundle. The `Task` and `TaskCreate`
  mirrors, the chat types and the webui types stay hand-written for now.
  [docs/HTTP-CONTRACTS.md](../HTTP-CONTRACTS.md) lists what phase 3/4 still
  has to convert.
- Component schemas are still hand-written in the base document. Generating
  them from the Go models by reflection is phase 4 completion work. The drift
  gate already does that reflection, so the move is mostly mechanical.
