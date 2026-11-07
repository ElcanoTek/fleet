# Manifest contract: reading fleet's API facts from the OpenAPI spec

ElcanoTek/manifest is an intake app that creates fleet tasks over the `/v1`
API. Its CI job `scripts/check-fleet-contract.mjs` (in the manifest repo)
checks a fixture of the fleet facts it depends on against a fleet checkout.
Today it finds most of them by grepping fleet's Go source for literal strings:
`Post("/tasks", h.CreateTask)` in `cmd/fleet/*.go`,
`r.Header.Get("X-API-Key")` in `batch.go`, `PermissionCreateTask` inside
`authorizeTaskCreator`, the `unversionedForever` `case` line in
`internal/apiversion`, and so on. The planned single-HTTP-surface refactor
(#2050) moves routing and auth, which will break those greps even though the
API stays the same.

This page is the hand-off for manifest's PR **M-1**. It lists each fact the
checker takes from fleet and the place in [`openapi.yaml`](openapi.yaml)
where M-1 should read it instead, and it says which facts the spec does
**not** carry yet, and why. The coupling doctrine still holds (AGENTS.md
"Repo Boundaries"): manifest owns the check, and fleet runs no reverse gate.
What fleet adds is a spec that CI tests against the code, so manifest can read
it instead of grepping Go source.

## What fleet guarantees about the spec

| Spec content | Proven by (`cmd/fleet`, run in CI on every PR) |
|---|---|
| The `{method, path}` set under `paths` equals the routes the real router registers | `TestOpenAPIRouteParity` |
| Every scheme named in an operation's `security` is defined | `TestOpenAPISecuritySchemesDefined` |
| `components.schemas.*` properties exist on the backing Go model, `required` ones are not `omitempty`, scalar types agree | `TestOpenAPISchemaDrift` (only spec ⊆ Go: a Go field missing from the spec is a warning, not a failure) |
| Every operation has `x-fleet-permission`, its value comes from the allowed set, `none` appears exactly when `security: []` | `TestOpenAPIPermissionDeclared` |
| **Each declared `x-fleet-permission` is the permission the code enforces** | `TestOpenAPIPermissionEnforcement` (needs the sched DB, `DATABASE_URL`, as CI sets it) |
| `x-fleet-api-version` equals `apiversion.Version`. A request under `servers[0].url` reaches the inner route and carries `X-Fleet-API-Version`. `GET <servers[0].url>/api-info` answers 200 with no credentials | `TestOpenAPIAPIVersion` |
| `x-fleet-key-types` equals `apikeys.KeyType.Permissions()` and `apikeys.TypeAllowsMethod` | `TestOpenAPIKeyTypes` |
| The API-key header the security schemes name is the header fleet reads. The enforcement probe sends every key under that header and every path under `servers[0].url`, so a wrong value fails the probe's admissions | `TestOpenAPIPermissionEnforcement` |

How the enforcement probe works: for every operation, it sends real requests
through `apiversion.Router(buildOrchestratorMux(...))` with real handlers on
a real database. It sends the bootstrap admin key, one typed key of each type,
a legacy `sk-` key holding exactly `{permission, view_tasks}`, and a legacy key
holding every permission **except** the declared one (and `admin`).

The probe expects:

- An "admitted" request gets neither 401 nor 403.
- A typed key that lacks the permission gets exactly 403.
- The legacy key that lacks the permission gets 401 or 403.

Request bodies are a minimal `{}`, so these refusals also show that body
validation never hides an authorization refusal. Manifest's
`/v1/tasks/estimate` key probe depends on that.

The probe excludes only the two A2A operations. A2A answers 501 until it is
enabled, and it reports per-method permission refusals as JSON-RPC errors
inside a 200 (its own tests in `internal/sched/handlers` cover those). The
probe also fails if any operation manifest reads is ever left out.

## The extensions

- **`x-fleet-api-version`** (top level, string): the HTTP API major version.
- **`x-fleet-permission`** (on every operation, one string): the
  `models.Permission` value an `X-API-Key` principal must hold to use the
  operation (`admin` implies every permission). Two other values exist:
  - `none`: the operation takes no API key (`security: []`).
  - `authenticated`: any valid key passes the HTTP layer. Only the A2A
    endpoint uses it.

  No operation accepts any one of several permissions, so the value is a
  single string, not a list. `info.description` in the spec gives the exact
  meaning, including what the value does not cover: per-row ownership rules,
  key-type method limits, the legacy-key baseline, and `GET /files/{filename}`,
  which accepts only the bootstrap key.
- **`x-fleet-key-types`** (top level): for each typed key class, the
  permission set it holds and the HTTP methods it may use.

The fixture names permissions by their Go constant names. The spec carries the
wire values:

| Go constant | Spec value |
|---|---|
| `PermissionCreateTask` | `create_task` |
| `PermissionViewTasks` | `view_tasks` |
| `PermissionCancelTask` | `cancel_task` |
| `PermissionViewLogs` | `view_logs` |
| `PermissionViewAllLogs` | `view_all_logs` |
| `PermissionManageKeys` | `manage_keys` |
| `PermissionAdmin` | `admin` |

## Fact → spec mapping (for M-1)

Each fact appears below in two path forms: a JSON Pointer, and a yq/jq path
over the parsed YAML. In a JSON Pointer, `/` inside a key becomes `~1`, so
`/tasks/{task_id}` is written `~1tasks~1{task_id}`. "Exists" means the key is
present, and `TestOpenAPIRouteParity` guarantees that the route is registered.

Checker section 1, "API surface: version, paths, auth":

| Checker fact (fixture key) | Read instead | Expected value |
|---|---|---|
| `api.apiVersion` | `/x-fleet-api-version` · `.["x-fleet-api-version"]` | `"1"` |
| `api.versionPrefix` | `/servers/0/url` · `.servers[0].url` | `"/v1"` |
| `api.apiInfoPath (route)` | `/paths/~1api-info/get` · `.paths["/api-info"].get` | exists |
| `api.createTaskPath` (`/v1` + POST `/tasks`) | `/paths/~1tasks/post` · `.paths["/tasks"].post` | exists |
| `api.uploadPath` (`/v1` + POST `/upload`) | `/paths/~1upload/post` · `.paths["/upload"].post` | exists |
| `api.estimateTaskPath` (`/v1` + POST `/tasks/estimate`) | `/paths/~1tasks~1estimate/post` · `.paths["/tasks/estimate"].post` | exists |
| `api.estimateTaskGate` ("same gate as create") | `/paths/~1tasks~1estimate/post/x-fleet-permission` and `/paths/~1tasks/post/x-fleet-permission` | both `"create_task"`, so check that they are equal |
| `api.apiInfoUnauthenticated` | `/paths/~1api-info/get/security` · `.paths["/api-info"].get.security` | `[]`. `.paths["/api-info"].get["x-fleet-permission"]` is also `"none"` |
| `api.authHeader` | `/components/securitySchemes/scopedApiKey/name` · `.components.securitySchemes.scopedApiKey.name` (and `.in`) | `"X-API-Key"`, `in: header`. `adminApiKey` names the same header |
| `api.createAcceptsScopedCreateKey` | `/paths/~1tasks/post/x-fleet-permission` · `.paths["/tasks"].post["x-fleet-permission"]` | `"create_task"`. `.security` also lists `scopedApiKey` |
| `api.uploadAcceptsScopedCreateKey` (fleet#719 pin) | `/paths/~1upload/post/x-fleet-permission` · `.paths["/upload"].post["x-fleet-permission"]` | `"create_task"`. #719 has shipped: `HandleUpload` validates scoped keys against `create_task` today, and the probe proves it |
| `api.unknownFieldsIgnored` | **not in the spec**. See the residuals below | — |

Permission and route facts from checker sections 7 and 7j:

| Checker fact | Read instead | Expected value |
|---|---|---|
| `deepAudit.outputPath` | `/paths/~1tasks~1{task_id}~1output/get` · `.paths["/tasks/{task_id}/output"].get` | exists |
| `deepAudit.taskPath` | `/paths/~1tasks~1{task_id}/get` · `.paths["/tasks/{task_id}"].get` | exists |
| `deepAudit.outputPermission` (`PermissionViewTasks`) | `/paths/~1tasks~1{task_id}~1output/get/x-fleet-permission` · `.paths["/tasks/{task_id}/output"].get["x-fleet-permission"]` | `"view_tasks"` |
| GET `/tasks/{task_id}` permission (not pinned today; M-1 may add it) | `.paths["/tasks/{task_id}"].get["x-fleet-permission"]` | `"view_tasks"` |
| `deepAudit.mcpAccountsPath` | `/paths/~1mcp-accounts/get` · `.paths["/mcp-accounts"].get` | exists. Its `x-fleet-permission` is `"view_tasks"` |
| `deepAudit.mcpServersPath` | `/paths/~1mcp-servers/get` · `.paths["/mcp-servers"].get` | exists. Its `x-fleet-permission` is `"view_tasks"` |
| `connectionCheck.logsPath` | `/paths/~1logs~1{task_id}/get` · `.paths["/logs/{task_id}"].get` | exists |
| `connectionCheck.logsPermission` (`PermissionViewLogs`) | `/paths/~1logs~1{task_id}/get/x-fleet-permission` · `.paths["/logs/{task_id}"].get["x-fleet-permission"]` | `"view_logs"` |
| `connectionCheck: a task key holds view_logs` | `/x-fleet-key-types/task/permissions` · `.["x-fleet-key-types"].task.permissions` | contains `"view_logs"` |

Three points about the permission facts:

- **Admission versus row access.** `x-fleet-permission` decides whether a key
  can use the operation. Once a key is admitted, task reads and transcripts
  are still limited to the rows that key created. Reading other keys' rows
  needs `view_all_logs` or `admin`. Manifest reads only tasks that its own key
  created, so the permission value is the fact it needs.
- **The value names the permission, not the gate function.** The grep version
  of `api.estimateTaskGate` pinned the gate *function*: `estimate.go` calls
  `authorizeTaskCreator`. The spec cannot name a function. Instead it states
  the gate's *effect*, the same `create_task` admission as POST `/tasks`, and
  the probe proves that effect. Manifest's probe also assumes that fleet
  authorizes **before** it reads the body, so that a 400 on a minimal body
  means the key was accepted. The enforcement probe covers that: it sends
  `{}`, and a refused key still gets 401 or 403, never 400.
- **Paths are relative to `servers[0].url`.** M-1 builds the client path as
  `.servers[0].url + <path key>`. The one exception is `/api-info`, which
  manifest calls at the bare path. The next section covers it.

## Facts already in spec schemas (for M-2, not M-1)

Manifest's later PR M-2 can move these too. The table says how far each is
covered today. Only `components.schemas.*` are drift-tested, and only in the
spec ⊆ Go direction. Inline response schemas and status codes are
documentation only.

| Checker fact | Spec location | Coverage |
|---|---|---|
| `taskCreate.fleetAcceptedFields[*]`, `deepAudit.taskCreateFields`, `deepAudit.manifestSentFields` | `.components.schemas.TaskCreate.properties` | **Partial.** Every fixture field is listed **except `allow_delegation`**. Go's `TaskCreate` also has fields the spec omits (`allow_event_triggers`, `run_if`, `sandbox_limits`, `loop_config`, `thinking_budget_tokens`, …); the fleet#720 gap is only partly closed |
| `taskCreate.serializationKeyHonored` | `.components.schemas.TaskCreate.properties.serialization_key` | Present and drift-tested to exist on the Go model. That a key is *enforced* at claim time is described in prose only ([`TASK-SERIALIZATION.md`](TASK-SERIALIZATION.md)) |
| `taskCreate.mcpChoiceFields` (MCPChoice) | `.components.schemas.MCPChoice.properties` | `server`, `account`; drift-tested |
| `taskCreate.mcpChoiceFields` (CredentialAllowlistEntry) | `.components.schemas.TaskCreate.properties.credential_allowlist.items.properties` | `server`, `account`; inline, so documentation only |
| `upload.multipartField` | `.paths["/upload"].post.requestBody.content["multipart/form-data"].schema.properties.file` | Present (documentation only) |
| `upload.responseFilenameField` | `.paths["/upload"].post.responses["200"]` | **Missing**: the 200 response has no schema, so `filename` is not documented |
| `deepAudit mcp-accounts` response key + `MCPAccountEntry` fields | `.paths["/mcp-accounts"].get.responses["200"].content["application/json"].schema` | `accounts[]` with `server`, `account`; inline, documentation only |
| `deepAudit mcp-servers` response key + `MCPServerCatalogEntry` fields | `.paths["/mcp-servers"].get.responses["200"]…schema` | `servers[]` with `name`, `enabled`, …. **Missing `always_on` and `remote`**, which the checker pins |
| `deepAudit.outputStatus` (200/404/409/410) | `.paths["/tasks/{task_id}/output"].get.responses` | Codes listed (documentation only). Which branch returns 409 and which 410 is not machine-readable |
| `deepAudit.terminalStatuses` | `.components.schemas.Task.properties.status.enum` | **Wrong today**: the enum is `scheduled, pending, running, completed, failed, cancelled`, while the Go `TaskStatus` values are `success`, `error`, `dead_lettered`, `leased`, `paused_*`, …. Enum values are not drift-tested |
| `deepAudit.tagAllowedChars`, `tagMaxLength`, `titleMaxChars`, `promptMaxBytes`, `schemaLimits.*` | `TaskCreate.properties.{tags,title,prompt,output_schema}.description` | Stated in prose (`≤64 chars`, `max 120 characters`, `3 to 250000 bytes`, `65,536 … 32 … 2,048`) but not as `maxLength`/`pattern` keywords, and not tested |

Fixing these gaps (adding `allow_delegation`, the upload response schema,
`always_on`/`remote`, a correct status enum, real `maxLength`/`pattern`
keywords, and moving the inline shapes to tested `components.schemas`) is
fleet-side work for M-2. This PR does not include it.

## Checks that cannot come from the spec

These checks either are not facts about the HTTP API, or the spec has no
honest way to state them today. M-1 should keep them as source pins, or as
`asserted-not-extracted` residuals:

- **`api.unknownFieldsIgnored`** (`readJSON` has no `DisallowUnknownFields`).
  This is decoder behaviour. A missing `additionalProperties` is the JSON
  Schema default, but the drift tests do not test it, so it would claim more
  than fleet proves. It could become a tested extension later.
- **`api.apiInfoUnauthenticated`, the "unversioned forever" half.** The spec
  proves that `/api-info` needs no credentials (`security: []`, plus the
  probe's 200 at `servers[0].url`/api-info). It does not say in a
  machine-readable way that the **bare** `/api-info` path is permanent and
  never deprecated: `servers[1]` (`/`) is described as legacy as a whole. That
  is in `api-versioning.md` prose and `apiversion.unversionedForever`.
  Manifest calls the bare path, so either keep that pin or switch the client
  to `/v1/api-info`, which is equivalent and tested.
- **Route-to-handler identity** (`h.CreateTask`, `h.GetTaskOutput`,
  `h.GetLogs`, …). The spec does not name handlers. Route parity plus the
  permission probe cover what the handler names were standing in for.
- **Validation error strings** (`taskCreate.fileNamesPairRule`,
  `duplicateFileNameError`). These are response text, not schema.
- **The ordering inside GetTaskOutput** (409 = not terminal yet, 410 = ended
  without output). That is behaviour inside the handler.
- **Agent-runtime semantics that are not HTTP**: the credential-allowlist
  denial marker and its nil/empty/populated semantics
  (`internal/agentcore/credential_allowlist.go`); structured-output strict
  mode; the EXECUTION REQUIREMENTS marker, roster, JSON keys and limits
  (`internal/sched/models/execution_requirements.go`); the `confirm_audit`
  schema text and required arguments (`internal/agentcore/audit.go`); run-log
  message fields and the tool role (`internal/agentcore/session_log.go`;
  the spec's `LogSession` schema has no properties on purpose); MCP
  registered-name and account folding (`mcp_selection.go`, `creds.go`).
- **The trader-facing task URL prefix** (`/orchestrator/tasks/`,
  `internal/runner/notify.go`). This is a web-UI route, not part of the task
  API.
- Everything the checker verifies on manifest's own code or on the
  elcano-config bundle (sections 2, 4 and 6–8 manifest halves, the bundle MCP
  catalog and tools). Those do not touch fleet at all.

## Keeping this page honest

If a fleet change alters a fact in the M-1 table, the matching test fails in
fleet CI: route parity, the permission probe, the version test or the
key-type test. A change that alters a fact listed as documentation only or
"not in the spec" does not fail any fleet test. That is why those facts stay
on manifest's side as pins or residuals. When fleet moves one of them into a
tested part of the spec, update its row here in the same PR.
