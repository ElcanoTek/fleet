# ADR-0007: Governed sub-agents spawn only through the one run loop

- **Status:** Accepted
- **Date:** 2026-06-28
- **Deciders:** fleet maintainers

## Context

Issue #175 asks for two capabilities in the native agent: a one-time "phone a
friend" super-LLM review (shipped in part a) and **sub-agents** — letting a run
delegate a scoped subtask to a child agent with its own model choice. Sub-agents
are the dangerous half: a child is a full agent that calls tools, spends money,
and touches credentials. The naïve implementation — spawn a fresh loop, or a
goroutine that calls the model directly — would create exactly the "different kind
of agent" that [ADR-0001](0001-one-governed-run-loop.md) forbids: a second,
weaker governance path, plus an unbounded way to multiply spend (a fan-out /
recursion fork-bomb) and to escalate privilege (a child reaching a credential or
network posture the parent could not).

[ADR-0001](0001-one-governed-run-loop.md) already anticipated this: *"Features
that feel like 'a different kind of agent' (sub-agents, review agents, channel
bots) must be expressed as configuration of, or adapters around, `agentcore.Run`
— not as a parallel loop."* This ADR records how sub-agents satisfy that
constraint, and the additional invariants their power demands.

## Decision

A sub-agent is **another `agentcore.Run`, governed exactly like its parent.** The
`spawn_subagent` native tool (`internal/agent/subagent.go`) only adapts I/O around
a fresh `agent.Agent.Execute` (→ `agentcore.Run`). It is **OFF by default**,
gated behind `FLEET_SUBAGENTS_ENABLED` (mirroring `FLEET_PHONE_A_FRIEND_ENABLED`);
when off the tool is not even registered. When on, every spawn obeys four
non-negotiable properties:

1. **Governance is one core.** The child runs through `(*Agent).Execute`, the same
   governed entrypoint the conformance test pins. No second loop, no second
   policy path, no privileged executor.

2. **Monotonic privilege.** The child inherits the parent's sandbox (so it shares
   the parent's network-seal posture — it has no namespace of its own to widen),
   the parent's brokered MCP client, and the parent's MCP/credential allowlists,
   and may only **subtract** (an `allow_servers` request is intersected with the
   parent's loaded set; the credential allowlist is the parent's, copied). A
   per-child model is resolved **host-side** (like the phone-a-friend reviewer),
   so credentials never enter the sandbox or model context.

3. **Hard budget split.** The child's cost/token ceiling is **sliced from the
   parent's remaining budget**, and the child's actual spend is **charged back**
   into the parent after it returns. The parent ceiling is therefore a hard wall
   that the collective spend of all descendants — across fan-out *and* depth —
   can never breach.

4. **Recursion / fan-out caps.** A small `maxDepth` (default 2) bounds recursion
   and a `maxChildren` (default 4) bounds per-parent fan-out. A spawn exceeding
   either is **refused with a tool error** — never a panic, never a silent allow.

## Enforcement

- `internal/agentcore/entrypoint_conformance_test.go` (`TestEntrypointConformance`)
  pins that `internal/agent/scheduled.go` — the file the child's `Execute` lives
  in — calls `agentcore.Run`, so the child cannot drift onto a forked loop.
- **Budget split:** `internal/agent/subagent.go` `spawn()` reads the parent's
  remaining budget via `(*agentcore.ScheduledPolicy).Budget` and charges child
  spend back via `(*agentcore.ScheduledPolicy).ChargeChildUsage`. The slice math
  (`sliceCostBudget`/`sliceTokenBudget`) hard-caps a grant at the parent's
  remaining budget. The child's own slice is enforced by the SAME
  `orchestrationState.checkCeilings` / `budgetGuardedStep` the parent uses.
  Tests: `TestSpawn_BudgetNeverExceedsParentCeiling`,
  `TestChargeChildUsage_FoldsIntoParentCeiling`,
  `TestSliceBudget_HardCapsAtParentRemaining`.
- **Depth / fan-out caps:** `spawn()` checks `subagent.depth >= maxDepth` and
  reserves a fan-out slot under the parent lock (`reserveChildSlot`). Tests:
  `TestSpawn_DepthCapRefusesAtMaxDepth`, `TestSpawn_FanOutCapRefusesExtraChild`.
- **Monotonic privilege:** `narrowedCredentialAllowlist` (copy, never widen) and
  `childSelection` (intersection, never union). Tests:
  `TestSpawn_AllowServersOnlyNarrows`,
  `TestSpawn_ChildRunsThroughGovernedCoreWithSlicedBudgetAndDepth`.
- **Off by default:** `config.SubagentsEnabled` defaults false; the tool is
  registered only when enabled (`Execute`). Test:
  `TestExecute_RegistersSpawnToolOnlyWhenEnabled`.

This ADR **extends** ADR-0001 rather than superseding it: it does not weaken the
one-governed-loop invariant, it adds the privilege/budget/recursion constraints
that make a *governed* child safe.

## Consequences

- Any safety gate added to `agentcore.Run` (a new approval, a new ceiling) applies
  to children automatically, because a child IS a governed run.
- The parent's configured `MaxCostUSD` / `MaxTotalTokens` remain the true cost
  bound for an entire spawn tree — operators size one number, not a per-child
  budget.
- A child cannot escalate: the worst a model can do via `spawn_subagent` is run a
  weaker, smaller-budget copy of itself, bounded by depth and fan-out.
- The feature is invisible until an operator opts in, so the default deployment is
  byte-for-byte unchanged.
- Cost: per-child accounting is charge-back, not a shared live ledger, so a
  child's spend becomes visible to the parent's ceiling only when the child
  returns (see "Alternatives"). For the bounded fan-out fleet supports this is an
  accepted, conservative trade.

## Alternatives considered

- **A shared, live budget ledger** both parent and child mutate in real time.
  Rejected for now: it would require threading a mutable ledger through
  `agentcore.Run`'s seams, for a marginal gain. `spawn_subagent` is registered as
  a **sequential** tool (`NewAgentTool`, `parallel:false`), so fantasy serializes
  spawns within a round behind its `sequentialMu`: each spawn's budget read and
  charge-back complete before the next spawn begins. The split is therefore
  **exact** — a sibling spawn always sees the prior sibling's spend already
  charged in. A child can still overspend its OWN sliced ceiling by at most one
  in-flight step (the gap between two `checkCeilings`), but that overrun is
  bounded by the child's slice, not the parent's, and is itself charged back; it
  cannot make the parent's *total* exceed the parent ceiling beyond a single
  child's last-step overrun. The depth/fan-out caps keep that small.
- **A `spawn_subagent` MCP server** (like lifeline). Rejected: it would push
  child execution toward the broker/sandbox boundary and create a second
  model-invocation path the policy does not govern — the same reasoning that kept
  phone-a-friend a host-side finish gate rather than a tool.
- **A goroutine that calls the model directly.** Rejected outright: it is the
  forked, ungoverned loop ADR-0001 exists to forbid.
