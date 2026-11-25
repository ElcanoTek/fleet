# HTTP contracts: what is generated

Issue #2050, phase 4 (and the phase 3 router it walks). The decisions and
their reasons are in [ADR-0109](adr/0109-generated-http-contracts.md) and
[ADR-0110](adr/0110-one-http-surface.md). This page covers what is generated
today, how to change it, and what phase 4 still has to do.

## Generated today

| File | Generated from | By |
|---|---|---|
| `docs/openapi.yaml` (from the generated-marker line down) | the routes the one public router registers under `/v1` (`buildPublicRouter` with the real `buildOrchestratorMux` mounted, walked with `chi.Walk`, `Family.Prefix = "/v1"`) joined with the route catalog `internal/apispec/orchestrator/operations.go` | `internal/apispec.Generate`, run by `TestOpenAPIGenerated` (`cmd/fleet`) with `-update` |
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

## Error responses

An error answer is either a refusal the reader can act on or an internal
error. This holds for the chat API (`internal/httpapi`), the browser gateway
(`internal/webui`), the orchestrator handlers (`internal/sched/handlers`) and
the binary's own routes and probes (`cmd/fleet`).

- **A refusal** carries the handler's own sentence: a validation failure, a
  decode error, a typed refusal ("a turn is running in this chat; stop it or
  wait for it to finish, then transfer", a store input error such as
  "nothing to change"). It is almost always a 4xx. The chat API answers it as
  a `text/plain` body, the orchestrator as `{"detail": "…"}`.
- **An internal error** is a failure the reader cannot act on: a store,
  driver, filesystem or network error. The response never repeats what the
  failing component said. It is logged, and answered with a fixed message.
  The log record differs by layer:

  | Layer | Log message | Fields | Answer |
  |---|---|---|---|
  | Chat API (`writeInternalError`, `logInternalError` in `internal/httpapi/internal_error.go`) | `internal error answered with a fixed message` | the request's correlation ids (`request_id`, `user_id`, …), `method`, `route`, `at` (the handler and line that hit the error), `err` | 500, body `internal error` |
  | Orchestrator handlers | per route ("update task tags failed", …); batch and import records: `internal error answered with a fixed message`; A2A: `A2A: internal error answered with a fixed message` | the request's correlation ids, `err` | the route's fixed `detail` ("Failed to update tags"), a record's `error`, or an A2A error, each `internal error` where no sentence of its own fits |
  | Browser gateway | per case (`model catalog: internal error answered with a fixed message`, `gateway: backend request could not be built`, `index: bootstrap could not be encoded`) | the request's correlation ids, `err` | 502 "the model catalog could not be loaded" / "chat-server unreachable" / "orchestrator unreachable", or 500 `internal error` |
  | `cmd/fleet` | `config reload failed`, `model tier reload failed`, `mcp reload failed`, `readiness: database ping failed`, `manage_tasks: a task change could not be saved` | the request's correlation ids, `err` | "… reload failed; the server log has the reason", the `/readyz` detail "database ping failed; the server log has the reason", the manage_tasks detail "the change could not be saved; try again" ("the task no longer exists" for a task deleted meanwhile) |

- **Fixed 5xx sentences.** A few 5xx answers keep a sentence of their own
  because it tells the reader what to do: the server shutting down or
  reconnecting (503), a workspace that could not be prepared, "the message
  could not be recorded against its turn; send it again", "transfer failed",
  "the change was partly applied: …; reload", a setting whose undo write
  failed ("… (and rolling back the persisted row failed — run Reset)"), and
  the partial-success answers ("user created, but assigning
  role/team/Operations Center access failed"). A path that picks its status
  at run time passes the error through `failureText`: it keeps a 4xx's text
  and one of these sentences, and treats any other 5xx as an internal error.
- **Typed refusals with a status of their own.**
  - A setting value the apply hook refused for the current configuration
    (`settings.ApplyError`, "guardrail mode block requires guardrail_url…")
    answers 409 with the hook's text.
  - Notification secrets the current key cannot decrypt answer 409 with what
    to do.
  - Input sent to a live browser session that has ended answers 410 "the
    browser session has ended". Input the page refused
    (`playwrightrunner.CommandError`) answers 422 with the worker's report,
    credentials masked.
  - A remote MCP failure keeps its redacted text (400) unless it is a
    database error (`IsDatabaseError`: a server error, a failed or broken
    connection, a closed pool), which is an internal error.
- **JSON fields.** A setting write's `rollout_error` and a rollout's
  `partial_error` carry fixed sentences, or `internal error`. A batch or
  import record's `error` carries a create refusal's text, or
  `internal error` for a store failure. A failed administrator request
  records a store refusal's text, "the chat no longer exists", or
  `internal error`. An A2A error that wraps no wire sentinel says
  `internal error`.
- **Stream frames.** `turn.error`'s `message` is one of these:
  - the text of a refusal written for the sender (`agent.TurnRefusalError`: a
    model lockdown does not allow, one with no provider route (with how to
    pick another), or a Lockdown chat on a server with no sandbox image);
  - "the workspace is at capacity right now — please resend your message in
    a moment";
  - "conversation history could not be saved";
  - "content blocked by workspace guardrail";
  - otherwise "the turn ended unexpectedly due to an internal error".

  `summary.error`'s `message` is the credits message, a refusal's text (a
  lockdown model, or one with no provider route), `summaryStaleMessage`, "the
  summary could not be saved; try again", or "summarizing failed; try
  again". The failure itself is logged.
- **Translation.** Every fixed sentence shown in the web UI is in the
  browser's message catalog (`web/src/app/lib/i18n/backend-messages.json`)
  with its Spanish and Portuguese translations, so `localizeError` shows it
  in the reader's language.

`TestNoErrorTextInResponses` (`internal/httpapi/error_text_guard_test.go`)
keeps error text out of these places. It fails when `x.Error()`, or an error
value formatted with `fmt.Sprintf`, reaches one of them:

- a 5xx `http.Error` or `writeError`, or one whose status is chosen at run
  time;
- a JSON body;
- an `error`, `message` or `detail` field outside a constant-4xx write
  (except in the files of two CLI verbs, `fleet validate-config` and
  `fleet mcp test`, whose details are terminal output);
- a stream frame;
- an A2A error response with the internal or server error code.

It has a short allowlist, with the reason for each entry. It sees only what
is written at the call: text passed through a variable is the reviewer's to
check.

## Phase 3 shipped: one router, one listener

`cmd/fleet/router.go` (`buildPublicRouter`) is one chi tree on the public
listener (`FLEET_WEB_ADDR`). It mounts the orchestrator router at `/v1` and
serves the unversioned-forever orchestrator paths and chat's `/webhooks/*`
with exactly the handlers their own listeners serve. `/api/orchestrator/*` is
the same orchestrator router behind the gateway's browser boundary
(`internal/webui/orchestrator_mount.go`); its rows in `routes.go` are gone.
`/config` and `/me` are explicit renames, and the old rows' query allow-lists
are `orchQueryPolicy`. Everything else is the gateway. The loopback chat and
orchestrator listeners stay up with the same handlers (the box Caddyfile and
the local CLIs use them). ADR-0110 has the invariant and its tests.

- `TestOpenAPIGenerated` walks the public tree with `Family.Prefix = "/v1"`.
  The generated document did not change.
- `TestBrowserAliasMountsTheV1RouteSet` asserts that `/api/orchestrator`
  registers exactly the `/v1` route set.
- `TestOneSurfaceParity` replays the browser route table captured before the
  change (`cmd/fleet/testdata/http-surface-before.txt`) through the new tree.

## What phase 4 still has to do

1. **Done in phase 3:** the unified router is walked once, the alias test
   exists, and the orchestrator rows of `internal/webui/routes.go` are gone.
2. **Chat (`internal/httpapi`, about 70 routes, `net/http.ServeMux`).** Phase
   3 mounts its public paths (`/webhooks/*`) on the tree, but its
   registration is still a `ServeMux` of prefix patterns that dispatch
   sub-paths inside handlers, and its browser routes are per-route
   translations (body rules, response shapes) rather than a prefix alias, so
   they stay gateway rows. It can be walked once its registration moves to
   chi. It needs an
   `internal/apispec/chat` catalog and base document and a `docs/` output,
   either a second document or a second family merged into one. The
   streaming routes need their SSE event shapes described. Their wire
   contract is pinned today by `testdata/contracts/chat-stream`.
3. **webui's own routes** (auth, SSO, theme, brand, uploads): the same, once
   the gateway's `ServeMux` registration moves to chi.
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
