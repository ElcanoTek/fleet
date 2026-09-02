# ADR-0056: Outbound A2A delegation as bundle-declared tools over the MCP seam

- **Status:** Accepted
- **Date:** 2026-09-02
- **Deciders:** fleet maintainers (issue #1368, the #1279 Phase 3)

## Context

ADR-0051 made fleet **callable** as an A2A agent. #1368 makes it a **caller**:
a fleet agent delegates work to a remote A2A server — another fleet, or any
A2A v1.0 agent — and collects the outcome. That adds an outbound network
surface driven by a model, credentials for remote peers, and a new class of
model-visible content (another agent's output), so four invariants have to be
decided rather than inherited.

Two prior decisions bound the design. **Governance is one core** (ADR-0001):
a new capability adapts I/O around `agentcore.Run`; it never forks a second
loop or a weaker tool path. And **bundles are data, fleet is engine**
(AGENTS.md Repo Boundaries): which peers exist is customer configuration, not
fleet code.

## Decision

1. **The delegation surface is a synthetic MCP server, not a native tool.**
   Peers declared in the bundle's `a2a_peers:` section register onto the
   credentialed `*mcp.Client` as one synthetic server (`_a2a`), exactly like
   inline `http_tools` (`_http`), contributing four tools per peer
   (`<peer>_send`, `_status`, `_wait`, `_cancel`). They therefore inherit the
   whole governance chain — policy gate, `governToolOutput` redaction and
   guardrail screening, the critical-tool gate, the model-visible output cap,
   the broker's child-side authorization (ADR-0042) — with no new path and no
   new loop. `internal/a2a` gains a thin JSON-RPC client that reuses the
   vendored wire types (the SDK's own client is not adopted: its encoding is
   Go-internal and its defaults do not fit the posture below).

2. **Peers are operator policy; a remote agent card never enters the model's
   view.** The bundle names the peer, its RPC URL, and a bundle-authored
   description — the ONLY text about the peer the model sees. Fleet does not
   fetch a remote agent card into the tool roster or system prompt: card text
   is authored by the remote party and would be a prompt-injection channel
   into every run that lists the peer. Discovery, when it comes, is an
   operator-side validation aid, not roster content.

3. **What a peer sends back is untrusted external content.** Remote status
   text and artifacts sit in the same trust class as a `web_fetch` body.
   Every rendered result opens with an explicit banner saying so, text parts
   are inlined under a byte cap, and file parts are described (name, media
   type, URI) — **never downloaded**. Landing remote files in the calling
   task's workspace is deferred: the tool runs host-side in the MCP client or
   broker process, which has no sandbox workspace handle, and the peer
   credential needed to fetch a file must never enter the sandbox — so it
   needs a host→workspace staging seam of its own.

4. **Peer credentials stay host-side, and the outbound posture is
   SSRF-guarded.** Headers carry `${ENV_VAR}` references resolved from the
   host process env at call time (the http_tools boundary): never in the
   sandbox, the model context, results, or logs. Every connection goes
   through `netguard`'s resolve-then-dial guard and refuses redirects
   unconditionally (a 30x must never relay a credential to another origin);
   every body is read through a hard byte cap. `FLEET_A2A_CLIENT_ALLOW_PRIVATE`
   relaxes only the dial guard, for dev/test rigs against loopback peers.

5. **Fleet-to-fleet recursion is bounded cooperatively.** An outbound send
   carries `X-Fleet-A2A-Depth: <own depth + 1>`; the inbound A2A server refuses
   values past `FLEET_A2A_MAX_DELEGATION_DEPTH` (default 3) and stamps the
   accepted depth on the created task; the outbound tools refuse locally when
   the calling run is at the ceiling. The depth travels with the task row and
   with the broker scope policy, never with model-controlled input. This is
   protection against fleets looping through one another, not against an
   adversarial peer: a non-fleet peer drops the header and its chain restarts
   at depth one. Documented as such.

6. **No tool call blocks on a remote run.** `_send` returns the remote task id
   at once (`returnImmediately`); `_wait` is SSE-backed and bounded at 120s;
   the calling run's own iteration and cost ceilings keep governing. The
   remote side's cost is invisible to fleet by construction — the bundle
   author's per-peer `critical: true` (approval card in chat, `confirm_audit`
   accounting when scheduled) is the control for that.

## Consequences

- A new peer is a bundle edit under additive-first schema rules; the generic
  bundle ships none, so nothing changes by default.
- `internal/mcp` gains its first fleet-internal import (`internal/a2a`, for
  the wire client). Acyclic today; if that coupling ever bites, the transport
  moves behind an exported synthetic-server seam.
- The depth guard is a new task column (`a2a_delegation_depth`, migration
  067, registry pattern #1126): immutable provenance, excluded from upsert,
  tx-update, and export.

## Enforcement

- `internal/mcp/a2atool_test.go`: secrets never in the catalog or results;
  depth refusal before any network call; remote errors as `isError` results;
  artifacts rendered without download; reload-vs-call race under `-race`.
- `internal/a2a/client_test.go`: headers on the wire (version, depth,
  credential), `returnImmediately`, oneof decoding, error mapping, SSE
  semantics, byte caps, redirect refusal, and the loopback dial block with the
  guard on.
- `internal/clientconfig/a2apeers_test.go`: schema validation (name charset,
  http(s)-only, no userinfo, description required, namespace collisions, the
  reserved `_a2a` name), `${ENV}` survival on the env-file allowlist, the
  critical fold, scrub, and the generic bundle shipping zero peers.
- `internal/sched/handlers/a2a_delegation_depth_test.go`: absent header →
  depth 1, echo, ceiling refusal naming the knob, junk refusal, follow-ups
  ignoring the header; the registry drift tests cover the column.
- `cmd/fleet/mcp_broker_authz_test.go`: the depth survives the scope-policy
  nil-collapse into the broker child.
