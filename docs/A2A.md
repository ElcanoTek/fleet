# The A2A protocol server

fleet can be called **as an agent** over the A2A (Agent2Agent) protocol
(#1279): an external agent — a Claude/LangGraph/Semantic Kernel/ADK stack,
another fleet, anything speaking A2A — discovers fleet through an Agent Card,
delegates work with `SendMessage`, streams progress, and collects results.
Everything lands on fleet's existing governed task seam: **A2A is a
protocol-shaped translation of the "one create seam in, one outcome out"
contract, not a new seam** (see `docs/BUILDING-ON-FLEET.md`), and no A2A
request ever reaches a model except through the same `agentcore.Run` pipeline
as any scheduled task.

## Spec pin

This implementation is written against **A2A v1.0.1** (Linux Foundation), and
the pin lives in one place: `internal/a2a.SpecVersion`. The normative source
is `specification/a2a.proto` at that tag — deliberately not the prose spec:
its §4 data-model tables are macro-generated (empty in the raw markdown) and
its migration guide (`whats-new-v1.md`) contains at least six confirmed
factual errors against the proto (event key names, pagination field names,
fields that were never added). Bumping the pin is a deliberate PR that
re-verifies every mapping in `internal/a2a` against the new proto.

Wire types come from the official Go SDK's pure-types packages
(`github.com/a2aproject/a2a-go/v2/a2a` + `errordetails`), which import only
the stdlib and `google/uuid`. The SDK's `a2asrv` server framework is **not**
used: its `AgentExecutor` is an execution loop, and fleet has exactly one
governed loop (ADR-0001). Measured cost of the dependency: one `go.mod`
require and three `go.sum` lines — module pruning keeps the SDK's
grpc/protobuf/cobra references out of the build and the lockfile entirely
(`go mod why github.com/spf13/cobra` → not needed by the main module).

## Enabling it

Off by default. Set `FLEET_A2A_ENABLED=1` and restart; until then both routes
answer `501 {"error":"a2a_disabled"}`. Optional operator policy:

| Env | Meaning |
| --- | --- |
| `FLEET_A2A_ENABLED` | Serve the A2A card + JSON-RPC endpoint (bool, default false, malformed value refuses boot) |
| `FLEET_A2A_PERSONA` | Bundle persona every A2A-created task runs with (empty = deployment default) |
| `FLEET_A2A_MODEL` | Model every A2A-created task runs with (empty = deployment default) |

Persona, model, ceilings, connectors: **operator policy, never caller
choice** — the same posture as webhook triggers (`docs/EVENT-TRIGGERS.md`).
An A2A caller sends messages, not configuration.

Set `FLEET_PUBLIC_BASE_URL` so the Agent Card's endpoint URL and artifact
file URLs are absolute; without it they are server-relative (fine when
callers reach the orchestrator at the same origin they fetched the card from).

## Surfaces

- `GET /.well-known/agent-card.json` — the discovery document. Public by
  construction (capabilities + endpoint URL + branding name; no secrets),
  spec-fixed path, unversioned forever (never Deprecation-tagged), served
  with `ETag`/`Cache-Control`.
- `POST /a2a` (canonically `/v1/a2a`) — the JSON-RPC 2.0 binding. Streaming
  methods answer `text/event-stream` where every `data:` line is a complete
  JSON-RPC envelope reusing the request id.

Requests must carry `A2A-Version: 1.0`. Spec §3.6.2 is implemented
literally: an absent header means protocol 0.3 and is refused with
`-32009 VersionNotSupportedError` naming the fix. Strict by design — an
integration that breaks here would have broken subtly later.

### Authentication

Exactly what the card declares: a fleet API key in `X-API-Key` — typed keys
(`fleet task-keys create`; `fleet_task_…` creates and reads its own work,
`fleet_readonly_…` reads) or the bootstrap admin key. Credential failures are
transport-layer 401/403; everything after auth is a JSON-RPC envelope. There
is deliberately no cookie or bearer path on `/a2a`, which is what makes its
CSRF exemption sound (`internal/sched/handlers/middleware.go`).

Authorization is per JSON-RPC method **inside** the dispatcher — the
HTTP-verb key scoping used on REST routes can't see which method a JSON-RPC
body carries (a readonly key must be able to `GetTask` through a POST).

### Method map — every row an existing seam

| A2A method | fleet seam |
| --- | --- |
| `SendMessage` | the shared create pipeline behind `POST /tasks` (`createTaskGoverned`: validate → run_if gate → budget gate → attribution → priority cap → persist); with `message.taskId`, the `POST /tasks/{id}/resume` seam |
| `SendStreamingMessage` | the same, then the task-row watcher (below) |
| `GetTask` | `GET /tasks/{id}` semantics, ADR-0043 creator-scoped |
| `ListTasks` | `GET /tasks` with the same SQL-side visibility filter; cursor pagination (`pageToken`), `nextPageToken` always present (`""` at the end) |
| `CancelTask` | `CancelTaskAtomic` + the live-run stopper (#508); idempotent on an already-cancelled task, `-32002` on other terminal states |
| `SubscribeToTask` | the task-row watcher; `-32004` if the task is already terminal (the result is `GetTask`'s to serve) |
| push-config methods | `-32003` (declared off, Phase 2) |
| `GetExtendedAgentCard` | `-32004` (declared off, Phase 2) |

Unknown/invisible tasks answer `-32001 TaskNotFound` — never 403 — so task
existence doesn't leak across keys (A2A §3.3.2 and ADR-0043 agree here).
Every A2A error carries the spec-required `google.rpc.ErrorInfo` detail
(`error.data` is an array of `@type`-tagged objects; `domain:
"a2a-protocol.org"`).

One deliberate authorization extension, scoped to this surface: **an API key
may cancel (and answer) a task it created.** On the REST surface a key never
gains cancel through ownership; on A2A, the caller that created the task is
exactly the credential the card tells integrators to use, and a cancel method
that credential can never pass would be dead on arrival. Recorded in
ADR-0051.

### State mapping

| fleet status | A2A `TaskState` |
| --- | --- |
| `pending` / `scheduled` / `leased` | `TASK_STATE_SUBMITTED` |
| `running` | `TASK_STATE_WORKING` |
| `paused_awaiting_wake` | `TASK_STATE_WORKING` (A2A has no "sleeping"; the caller needs to do nothing) |
| `paused_awaiting_input` | `TASK_STATE_INPUT_REQUIRED` — `status.message` carries the question; answer by `SendMessage` with the same `taskId` |
| `success` | `TASK_STATE_COMPLETED` |
| `error` / `dead_lettered` | `TASK_STATE_FAILED` (retry exhaustion is an implementation detail) |
| `cancelled` | `TASK_STATE_CANCELED` — `status.message` carries the stop attribution |

Exhaustive by test: `internal/a2a/mapping_test.go` ranges
`models.AllTaskStatuses`, so a new fleet status cannot silently map to
nothing. `TASK_STATE_REJECTED` is never stored (fleet refuses at admission,
before a row exists) and `TASK_STATE_AUTH_REQUIRED` has no fleet equivalent —
a `ListTasks` filter on either legitimately matches nothing.

### Results and artifacts

A terminal task renders its outputs as A2A artifacts: `result` (the free-form
answer, text part), `output` (the schema-validated `output_json`, data part),
and one artifact per published workspace file — a **URL part pointing at the
authenticated workspace endpoint** (`/v1/tasks/{id}/workspace/…`), fetched
with the same `X-API-Key`. Artifact URLs are not bearer capabilities. Task
`history` is deliberately empty: fleet transcripts are tool-call run logs,
not A2A `Message`s, and pretending otherwise would misrepresent them.

### Streaming

A task-lifecycle stream opens with the `Task` snapshot, emits a
`statusUpdate` per state transition (plus `artifactUpdate`s at terminal), and
**closes when the task reaches a terminal state — closure is the completion
signal** (v1.0 removed the `final` flag). The event source is the task ROW,
polled once a second — not the in-memory run-log buffer. That is a deliberate
trade: the buffer would only shave sub-second latency off transitions that
are seconds apart, at the cost of merging two event sources, and it evicts
after two minutes while the row never lies. Consequence worth knowing: a
dropped connection loses nothing — `SubscribeToTask` re-reads the row — and
there is no `Last-Event-ID` protocol (the A2A spec has none either).

## Honest scope

- **JSON-RPC + SSE only.** The gRPC and HTTP+JSON bindings are not
  implemented; the card says so by declaring a single `supportedInterfaces`
  entry. Per spec §5.2 that is a complete, legal declaration.
- **Push notifications are Phase 2** (`pushNotifications: false`; the four
  config methods answer `-32003`). Fleet's outbound webhook config is
  deployment-wide today (`docs/NOTIFICATIONS.md`); per-task push needs
  per-task webhook storage plus SSRF-guarded delivery — real design work.
- **Extended agent card is Phase 2** (`-32004`).
- **Text parts only inbound.** `raw`/`url`/`data` parts answer `-32005`; the
  card declares `defaultInputModes: ["text/plain"]`. File intake exists on
  the REST seam (`POST /upload` + `files`), not on A2A yet.
- **The stream carries status + terminal artifacts**, not incremental agent
  text or tool events.
- **Callers cannot choose personas/models/ceilings** — operator env pins them.
- **`paused_awaiting_wake` reports as `WORKING`** (no better honest option in
  the A2A vocabulary).
- **`statusTimestampAfter` (ListTasks) is refused** (`-32602`) rather than
  silently ignored.
- **Task history is empty** (see above).
- **Spec-literal `A2A-Version` handling** — absent ⇒ 0.3 ⇒ `-32009`.
- **Fleet as an A2A *client*** (delegating outward) is out of scope — Phase 3,
  its own issue.
- This is also **not an MCP server**: fleet exposing its tools over MCP
  remains a separate (non-)feature.

## Conformance testing

The blocking gate is the Go test suite: golden-shape envelope tests and the
mapping exhaustiveness guard (`internal/a2a`, `internal/sched/handlers/a2a_test.go`),
plus the OpenAPI route-parity, CSRF-coverage, and knob-registry drift tests
that lock the mounting.

The official Python TCK (`a2aproject/a2a-tck`) runs locally against a live
server via `scripts/a2a-tck.sh` (uv + a pinned TCK commit;
`--transport jsonrpc --level must`). It is not a CI job in Phase 1: it needs
a Python toolchain and a booted server, and its own spec pin (v1.0.0) trails
the one this implementation uses. Run it before releases that touch the A2A
surface and attach `reports/compatibility.json` to the PR.

## Not a reopening of #183/#290

Those asked for durable shared state *between* fleet agents and were closed
out of scope. This is the opposite shape: one external caller in → one
governed, isolated run → one outcome out. No inter-task state, no channels,
no change to run isolation (ADR-0051).
