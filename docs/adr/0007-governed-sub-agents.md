# ADR-0007: Governed sub-agents spawn only through the one run loop

- **Status:** Accepted
- **Date:** 2026-06-28 (amended 2026-06-30 for #264)
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
a fresh `agent.Agent.Execute` (→ `agentcore.Run`). It is **OFF by default** and
turned on **per task** via `allow_delegation: true` OR **fleet-wide** via
`FLEET_SUBAGENTS_ENABLED` (the two compose as OR — the env flag is the operator
override, the task flag the granular opt-in; #264). When both are off the tool is
not even registered, and it is **only ever** registered in scheduled mode, never in
interactive chat. When on, every spawn obeys these non-negotiable properties:

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

3. **Hard budget split.** The child's cost/token ceiling is **capped at a fraction
   of the parent's remaining budget** (`FLEET_SUBAGENTS_BUDGET_FRACTION`, default
   `0.10`) and **sliced from what the parent has left**; the child's actual spend is
   **charged back** into the parent after it returns. A request above the per-child
   cap is **refused** (not clamped). The parent ceiling is therefore a hard wall
   that the collective spend of all descendants — across fan-out *and* depth —
   can never breach.

4. **One-level delegation + fan-out cap.** `maxDepth` (default `1`) means **parent →
   sub-agent only**: a child is built **without** the `spawn_subagent` tool (the
   primary, structural enforcement — non-registration, immune to an off-by-one in a
   counter), with the in-body depth check as a backstop. `maxChildren` (default `5`)
   bounds per-parent fan-out. A spawn exceeding either is **refused with an error
   result** — never a panic, never a silent allow, never a block.

## Enforcement

- `internal/agentcore/entrypoint_conformance_test.go` (`TestEntrypointConformance`)
  pins that `internal/agent/scheduled.go` — the file the child's `Execute` lives
  in — calls `agentcore.Run`, so the child cannot drift onto a forked loop.
- **Budget split (atomic):** `internal/agent/subagent.go` `reserveChildBudget`
  computes and reserves a child's ceiling **under the parent mutex (`a.mu`)** in
  one critical section: it reads the parent's remaining budget via
  `(*agentcore.ScheduledPolicy).Budget`, subtracts the budget already granted to
  in-flight siblings (`subagent.reservedCostUSD`/`reservedTokens`), slices a grant
  from what is genuinely available (`grantCostFrom`/`grantTokensFrom`, hard-capped
  at available), and adds the grant to the reservation. Because that
  read-modify-write is serialized by `a.mu`, the **sum of budgets granted to any
  number of concurrent spawns can never exceed the parent's remaining budget** —
  the hard wall does NOT depend on the tool being sequential. `releaseChildBudget`
  frees the reservation when the child returns; `ChargeChildUsage` then folds the
  child's ACTUAL spend into the parent. The child's own slice is enforced by the
  SAME `orchestrationState.checkCeilings` / `budgetGuardedStep` the parent uses.
  Tests: `TestSpawn_BudgetNeverExceedsParentCeiling`,
  `TestReserveChildBudget_ConcurrentNeverOverGrants` (fires N concurrent
  reservations, asserts the summed grant never exceeds remaining; passes under
  `-race`), `TestSpawn_ConcurrentNeverBreachesParentCeiling` (concurrent full
  spawns; passes under `-race`), `TestChargeChildUsage_FoldsIntoParentCeiling`,
  `TestReserveChildBudget_AtomicAndHardCaps`, `TestGrantFrom_HardCapsAtAvailable`.
- **Depth / fan-out caps:** `spawn()` checks `subagent.depth >= maxDepth` and
  reserves a fan-out slot under the parent lock (`reserveChildSlot`). Tests:
  `TestSpawn_DepthCapRefusesAtMaxDepth`, `TestSpawn_FanOutCapRefusesExtraChild`.
- **Monotonic privilege:** `narrowedCredentialAllowlist` (copy, never widen) and
  `childSelection` (intersection, never union). Tests:
  `TestSpawn_AllowServersOnlyNarrows`,
  `TestSpawn_ChildRunsThroughGovernedCoreWithSlicedBudgetAndDepth`.
- **Off by default:** `config.SubagentsEnabled` defaults false and no task opts in;
  the tool is registered only when enabled (`Execute`). Test:
  `TestExecute_RegistersSpawnToolOnlyWhenEnabled`.

## #264 amendment — agent delegation completed

Issue #264 ("agent delegation — spawn sub-agents for parallel work") was filed
before #175 landed and asked for a `delegate_task` tool. Because #175 already built
the governed delegation core, #264 is **completed by extending that one tool**, not
by adding a second `delegate_task` entrypoint (which would be the forked, weaker
path this ADR exists to forbid — a second registration, a second name in the audit
log, and an LLM-ergonomics hazard of two identical tools). The behavioural deltas,
all preserving the properties above:

- **Per-task opt-in.** A new `allow_delegation` task field registers the tool for
  that task even when `FLEET_SUBAGENTS_ENABLED` is off; they compose as OR. The
  env flag is **retained** as the fleet-wide operator override — a literal reading
  of #264 ("opt-in per task, not a global toggle") is satisfied because the
  per-task flag is *sufficient on its own*; the env flag is an additional override,
  not a precondition. Default deployments (both off) are byte-for-byte unchanged.
  The flag is threaded like `allow_network` (DB column, export/import, rerun
  overrides). Tests: `TestTaskAllowDelegationRoundTrip`,
  `TestExportImport_AllowDelegationRoundTrip`.
- **Parallel fan-out.** The tool is marked **parallel** (`NewParallelAgentTool`),
  so fantasy dispatches multiple `spawn_subagent` calls in one turn concurrently
  (its parallel-tool semaphore bounds true concurrency) and the parent collects all
  results before its next LLM call. The atomic reservation already made this safe;
  the marking is what lets fantasy drive it. Test:
  `TestSpawn_ParallelExecutionWallClock` (wall-clock ≪ sum of sequential).
- **JSON result.** The tool now returns `{result, cost_usd, tokens, success}` so a
  parent can branch deterministically on concurrently-returned results; refusals
  are `success:false` results, never a panic. Test: `TestSpawn_JSONResultShape`.
- **Default changes (all tighten or align governance).** `maxDepth` 2→**1**
  (children get no spawn tool — "parent → sub-agent only"), `maxChildren` 4→**5**
  (#264's "max 5"), and the per-child budget grant moves from a 50% default slice
  to a **10%** cap that *refuses* over-cap requests (`FLEET_SUBAGENTS_BUDGET_FRACTION`,
  configurable). Lowering depth also forecloses a latent deadlock against fantasy's
  shared parallel-tool semaphore.
- **Per-child `timeout_minutes` + `max_iterations`.** Optional bounds on a child's
  wall-clock and agent steps; the child ctx derives from the parent's, so a parent
  kill-switch cancels children too, and spend is charged back on every exit path
  (success, error, timeout, panic). Tests: `TestSpawn_TimeoutBranchAndChargeBack`,
  `TestBuildChild_MaxIterationsCappedAtParent`.
- **Traceability (`parent_task_id`).** A child's session log carries the owning
  task id and the parent's persisted log gains a `subagent_spawned` linkage entry
  with the child id + spend. Tests: `TestBuildChild_ParentTaskIDLinkage`,
  `TestRecordSubagentSpawn_AppendsToParentLog`.

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
- Cost: per-child accounting combines an **atomic up-front reservation** of each
  child's granted ceiling with **charge-back** of the child's actual spend on
  return. The grant is conservative (a child rarely spends its whole slice), so
  in-flight reservations can refuse a spawn that real spend would have allowed;
  this errs toward staying under the parent ceiling, which is the safe direction.
  A child's actual spend becomes visible to the parent's ceiling only when it
  returns, but its *reserved* budget is held against the wall the entire time it
  runs (see "Alternatives").

## Alternatives considered

- **A shared, live budget ledger** both parent and child mutate in real time
  (every child token charged the instant it is spent). Rejected: it would require
  threading a mutable, lock-shared ledger through `agentcore.Run`'s seams for a
  marginal gain over the reservation model. The over-grant risk this would solve
  is already **closed atomically**: `reserveChildBudget` holds each child's
  granted ceiling against the parent's remaining budget under `a.mu` for the whole
  time the child runs, so even N concurrent spawns can never collectively be
  granted more than the parent has left. This is **enforced atomically under the
  parent mutex and covered by a concurrency regression test**
  (`TestReserveChildBudget_ConcurrentNeverOverGrants` and
  `TestSpawn_ConcurrentNeverBreachesParentCeiling`, both run under `-race`) — it
  does **not** rely on `spawn_subagent` being a sequential tool. A child can still
  overspend its OWN sliced ceiling by at most one in-flight step (the gap between
  two `checkCeilings`), but that overrun is bounded by the child's slice, charged
  back on return, and capped by the depth/fan-out caps.
- **A `spawn_subagent` MCP server** (like lifeline). Rejected: it would push
  child execution toward the broker/sandbox boundary and create a second
  model-invocation path the policy does not govern — the same reasoning that kept
  phone-a-friend a host-side finish gate rather than a tool.
- **A goroutine that calls the model directly.** Rejected outright: it is the
  forked, ungoverned loop ADR-0001 exists to forbid.
