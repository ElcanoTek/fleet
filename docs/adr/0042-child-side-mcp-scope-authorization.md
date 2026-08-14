# ADR-0042: Child-side MCP scope authorization

- **Status:** Accepted
- **Date:** 2026-08-14
- **Deciders:** fleet maintainers
- **Amends:** ADR-0003 (host-side MCP credential brokering) — the broker gains
  an authorization boundary it explicitly did not have

## Context

The `fleet mcp-broker` boundary (issue #167) delivered *can't-read*: after the
child boots and passes liveness, tool discovery, and account discovery, the
parent scrubs every connector environment key, drops the resolved server
definitions, and can no longer obtain a bundle connector secret. That part is
done and is not revisited here.

It did not deliver *can't-exercise*. The broker transported but never validated:
`OpenScope` bound whatever `{server, account}` selection arrived, and a call
frame ran whatever `server.tool` it named. Every gate lived parent-side in
`agentcore` — the per-server tool allowlist (Gate-2), the per-task credential
allowlist (Gate-3, #184), persona narrowing (Gate-4) — that is, inside the
address space the boundary exists to distrust. `docs/MCP-BROKER-SCOPES.md` said
so deliberately: *"No authorization boundary is added here."*

The cost was demonstrated rather than hypothesised. Activating the production
boundary scrubbed `cfg.MCPServers`, which was exactly where the scheduled agent
read its Gate-2 allowlist from, so every scheduled run silently lost its tool
allowlist (#960). Nothing on the credential-owning side noticed, because nothing
on the credential-owning side was looking. A parent-side gating bug is a total
gating bypass.

## Decision

The credential owner enforces authorization itself, and the parent's gates
become narrowing input rather than the whole answer.

1. **Gate-2 is authoritative child-side.** The child derives the per-server tool
   allowlist and the enabled-server set from the bundle *it* loads, and
   re-derives both from the same bundle read on every reload. Neither is taken
   from the wire.
2. **The parent's effective sets cross as `ScopePolicy` and may only narrow.**
   Scope open carries the run's effective Gate-2 map and, for scheduled runs,
   the per-task Gate-3 pairs. The child intersects the tool sets with its own
   and applies Gate-3 through the same `agentcore.GateMCPBrokerWithAllowlist`
   the in-process loop uses. A parent that sends a wider policy — or none at
   all — gets the bundle floor, never more.
3. **A scope may only reach what it was bound to.** A selection naming a server
   the child does not have enabled is refused at open, before anything spawns,
   and a call naming a server outside the scope's registered set is refused at
   dispatch even if the underlying client would have routed it.
4. **A denied tool is never advertised.** The scope's returned catalog is
   filtered through the same gate that will judge its calls, so the parent's
   roster and the model's tool list cannot disagree with what dispatch permits.
5. **The unscoped shared client is restricted, not removed.** Production agent
   turns and scheduled runs fail closed without a scope, and approval execution
   reopens the seat it staged under, so no agent path reaches it. What remains —
   pre-existing approval rows that recorded no seat, and operator tooling — is
   still gated by the child's bundle allowlist and enabled set.

Refusals are tool-level errors (`isError=true`, no transport error) carrying the
stable `mcp_broker_policy_denied` marker and public names only, matching how the
in-process credential allowlist already denies a call: the model sees a
governance result and the caller records it for audit.

Persona narrowing (Gate-4) stays a parent-side registration filter. It decides
what a model may *see*, is resolved from persona definitions the parent owns,
and can only subtract from what the gates above already permit — so it is not a
credential boundary and is deliberately not duplicated in the child.

## Enforcement

- `cmd/fleet/mcp_broker_authz.go` holds the whole gate: the bundle floor, the
  narrowing intersection, the registered-name projection, and the selection
  check.
- `cmd/fleet/mcp_broker.go` applies it at scope open, on every scoped call, and
  on the unscoped shared-client path, and refreshes the floor inside `Reload`
  under the same lock that publishes new bases.
- `cmd/fleet/mcp_broker_authz_test.go` pins the load-bearing case — a parent that
  claims a tool the bundle denies is refused — plus parent-side narrowing,
  child-side Gate-3, the unknown-server refusal, the unscoped-path gate, and the
  account-variant name projection.
- `agentcore.RegisteredMCPName` is the one formula both sides project
  `{server, account}` through, so a named-account scope gates the key
  `BindMCPSelection` actually registered.

## Consequences

- A parent-side gating bug now restricts a run instead of unbounding it. The
  #960 failure mode — a parent that lost its allowlist entirely — degrades to
  the bundle allowlist rather than to "everything".
- Gate-2 is enforced twice, which is the point; the cost is that a bundle whose
  `tool_allowlist` disagrees with a parent's stale snapshot now surfaces as a
  refusal rather than a silent success.
- Gate-3 has no child-side source of truth and remains parent-supplied. A parent
  that omits it does not gain reach beyond the bundle floor, but it does lose
  the per-task narrowing — so this is defence in depth for Gate-2 and faithful
  relay for Gate-3, not equal assurance for both.
- This ADR makes no claim about a total compromise of the parent process's
  database or encryption key; see ADR-0040 and `SECURITY.md`.

## Alternatives considered

- **Trust the parent's policy outright.** Rejected: that is the status quo with
  extra JSON. If the wire snapshot were authoritative, the #960 class of bug
  would still be a full bypass.
- **Derive Gate-3 in the child too.** Rejected: the per-task credential
  allowlist is scheduler data in the parent's database, and giving the child a
  second reader of task rows would widen the child's surface to buy assurance
  the parent already has a reason to get right.
- **Remove the unscoped call path entirely.** Deferred: approval rows staged
  before migration 048 added the seat columns have no seat to reopen, and
  failing them closed would break in-flight cards on upgrade. The path is
  gated instead, and no agent turn can reach it.
