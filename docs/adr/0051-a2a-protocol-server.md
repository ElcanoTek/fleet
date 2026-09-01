# ADR-0051: An A2A protocol server as a translation over the governed task seam

- **Status:** Accepted
- **Date:** 2026-08-27
- **Deciders:** fleet maintainers (issue #1279)

## Context

External agent stacks increasingly interoperate over the A2A (Agent2Agent)
protocol, the delegation-standard sibling of MCP's tool standard. fleet's
external-integrator contract has always been "one create seam in, one outcome
callback out" (AGENTS.md Repo Boundaries item 2); #1279 asked for that same
contract wearing A2A's wire format, so an orchestrator points at fleet
instead of growing bespoke `POST /tasks` glue.

This adds an externally reachable surface, and two prior decisions bound the
design: **governance is one core** (ADR-0001 — a new entrypoint adapts I/O
around `agentcore.Run`, never forks a weaker loop), and **task reads are
creator-scoped** (ADR-0043). It must also not reopen #183/#290: A2A here is
one caller in → one isolated governed run → one outcome out, with no
inter-task state or channels.

## Decision

1. **The A2A server is a protocol translation, not a subsystem.**
   `internal/a2a` holds wire translation only (state/artifact mapping, card,
   JSON-RPC envelope); the dispatcher lives in `internal/sched/handlers` and
   calls only existing seams — `SendMessage` runs the same
   `createTaskGoverned` pipeline as `POST /tasks` (extracted so a second
   protocol surface cannot become a weaker create route; the
   `storage.EnqueueTask*` seams skip validation and were deliberately NOT
   used), reads go through the ADR-0043 predicates, cancel through
   `CancelTaskAtomic` + the live-run stopper, and streaming polls the task
   row. No A2A code touches `agentcore` or runs a model.
2. **Spec pin v1.0.1; the proto is the authority.** `internal/a2a.SpecVersion`;
   the prose spec's data tables and migration doc have confirmed errors, so
   every mapping cites `specification/a2a.proto`. Wire types are imported
   from the official SDK's pure-types packages (`a2a-go/v2/a2a`,
   `errordetails` — stdlib+uuid only). The SDK's `a2asrv` framework is
   rejected: its `AgentExecutor` is a competing execution loop. Measured
   dependency cost: one `go.mod` require and three `go.sum` lines — module
   pruning keeps the SDK's grpc/cobra references out of both the build and
   the lockfile (`go mod why github.com/spf13/cobra` → not needed; grpc was
   already linked via the OTel trace exporter, version unchanged).
3. **Auth reuses typed API keys, no new credential.** `X-API-Key` only — the
   credential the Agent Card declares. Authorization is per JSON-RPC method
   in the dispatcher (HTTP-verb scoping cannot see a multiplexed method).
   Invisible tasks answer `-32001 TaskNotFound`, never 403 (spec §3.3.2 and
   ADR-0043 agree). **One deliberate extension, scoped to this surface:** a
   key may cancel and answer tasks it created — on A2A, the creating key is
   the only credential the caller has, and a cancel method it can never pass
   would be dead protocol surface. The REST surface's rule (keys never gain
   cancel through ownership) is unchanged.
4. **Task configuration is operator policy.** `FLEET_A2A_PERSONA` /
   `FLEET_A2A_MODEL` pin what A2A tasks run with (webhook-trigger posture);
   callers send messages only.
5. **Off by default, fail-closed.** `FLEET_A2A_ENABLED` (typed knob registry:
   malformed refuses boot); unwired routes answer 501 but stay registered so
   the OpenAPI parity walk sees them. `/a2a` is CSRF-exempt because its auth
   never involves a browser-auto-sent credential — and the dispatcher
   enforces that by construction (no cookie/bearer path).
6. **Deferred honestly:** push notifications (`-32003`) and the extended card
   (`-32004`) per the spec's capability-gating rules — never MethodNotFound;
   gRPC/REST bindings undeclared; fleet-as-A2A-client is a separate issue.

## Enforcement

- `internal/a2a/mapping_test.go` — `TestTaskStateMappingIsExhaustive` ranges
  `models.AllTaskStatuses`; a new fleet status without a mapping fails.
- `internal/a2a/card_test.go`, `jsonrpc_test.go` — v1.0 card shape (required
  fields, absent v0.3 fields), the spec's error-code table, the
  `google.rpc.ErrorInfo` detail, spec-literal `A2A-Version` handling.
- `internal/sched/handlers/a2a_test.go` — creator scoping over the real
  routes/storage (`-32001`, not 403), governed-create attribution + persona
  pinning, capability gating, cancel semantics, the streaming lifecycle
  (snapshot first, close at terminal).
- `cmd/fleet/openapi_drift_test.go` (route parity) and
  `cmd/fleet/csrf_coverage_test.go` (the `/a2a` exemption is pinned with its
  rationale) — the mounting cannot drift silently.
- `internal/config/knobs_coverage_test.go` + the env-allowlist drift test —
  the `FLEET_A2A_*` knobs stay registered and validated.
- `internal/agentcore/entrypoint_conformance_test.go` — unchanged, which is
  the point: the A2A adapter added no entrypoint.

## Amendment — Phase 2 push notifications (2026-09-01)

Per-task push-notification configs shipped as scoped in the issue, three
security-relevant decisions on top of the original record:

1. **Caller webhook secrets are sealed in the sched DB** (`a2a_push_configs`,
   migration 066) with `internal/secretbox`, AAD-bound per row, under the
   existing store cipher (`FLEET_MCP_OAUTH_ENCRYPTION_KEY` — the same key
   docs/NOTIFICATIONS.md already treats as the generic store cipher, injected
   into `sched/db` via a `SetA2APushCipher` setter mirroring
   `SetLogArchiveKey`). No cipher ⇒ the capability is off, fail-closed: the
   card declares `pushNotifications: false` and the methods answer `-32003`.
2. **Delivery is SSRF-guarded by default**: push URLs are CALLER-supplied
   (the web_fetch trust class, not the operator-trusted `FLEET_WEBHOOK_URL`
   class), so the sender dials through the resolved-IP netguard check and
   refuses every redirect (`mcpoauth.SafeHTTPClient`).
   `FLEET_A2A_PUSH_ALLOW_PRIVATE` (default false) relaxes only the dial
   guard — required for the official TCK, whose receiver is on localhost —
   and boot logs a warning when set.
3. **Delivery is poll-based** (a 1s scan of configs whose task status moved
   past their per-config marker), not a Storage transition bus: the task row
   is already the A2A event source, and an observer seam would touch every
   lifecycle writer for latency the spec does not require. The marker is
   claimed BEFORE the POST (one-winner conditional update), so races and
   crashes err toward a missed doorbell — superseded by the next transition —
   never toward unbounded duplicates; the spec demands only at-least-once
   ATTEMPT and tells clients to expect duplicates anyway.

Enforcement additions: `internal/sched/db/a2a_push_test.go` (sealed-at-rest
round-trip, fail-closed without cipher, one-winner mark),
`internal/sched/handlers/a2a_push_test.go` (CRUD contract incl. the TCK's
snake_case params and client-id round-trip, inline registration, delivery
lifecycle with exact Authorization/token/media-type assertions, and the SSRF
guard blocking a loopback receiver by default).

## Consequences

- Any A2A-speaking stack can delegate to fleet with a typed key; every run is
  governed identically to a scheduled task (policy, ceilings, audit, sandbox).
- The status vocabulary is compressed where A2A is narrower than fleet
  (`paused_awaiting_wake`→WORKING, `dead_lettered`→FAILED) — documented in
  docs/A2A.md rather than widened with custom states.
- Streaming granularity is the 1s task-row poll; the in-memory run-log buffer
  is deliberately not merged in (two event sources for sub-second latency on
  transitions that are seconds apart; the row survives buffer eviction).
- `go.sum` grows by exactly the SDK's own module hashes (three lines);
  nothing beyond the two types packages is linked.

## Alternatives considered

- **The SDK's `a2asrv` server framework** — rejected: `AgentExecutor` is an
  execution loop with its own task store/queues, forking governance
  (ADR-0001).
- **Hand-rolled wire types** — rejected: the oneof `Part`/`StreamResponse`
  and SCREAMING_SNAKE enums are exactly where hand-rolling drifts; the SDK's
  types package carries them with a stdlib-only import reach.
- **Routing A2A creates through `storage.EnqueueTaskAs`** — rejected: it
  skips `validateTaskCreate` (the approval-card adapter hand-patches around
  that today); extraction of the shared pipeline fixes both surfaces.
- **Mapping `paused_awaiting_input` answers onto a new message channel** —
  rejected as #183-shaped; the existing resume seam already is the answer
  path.
