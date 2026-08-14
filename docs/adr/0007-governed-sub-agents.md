# ADR-0007: Governed sub-agents spawn only through the one run loop

- **Status:** Accepted
- **Date:** 2026-06-28 (amended 2026-06-30 for #264, 2026-08-14 for #1043:
  default-on, parent decides, typed children, interactive in scope)
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
a fresh `agent.Agent.Execute` (→ `agentcore.Run`). Since #1043 it is **ON by
default**: the fleet-wide flag (`FLEET_SUBAGENTS_ENABLED` / Admin → Features) and
the per-task `allow_delegation` column both default **true** and compose as
**AND** — the operator only ever opts **out**, per task or fleet-wide (two
independent kill switches). Interactive chat registers the same tool whenever the
fleet flag is on (chat has no per-conversation column). When the composed gate is
off the tool is not even registered — structural, not a soft check. Registering
the tool is the feature: the **parent agent decides** whether to spawn 0, 1, or N
children, and a sequential run that never delegates is a successful use of it.
When registered, every spawn obeys these non-negotiable properties:

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
- **Structural registration (default-on since #1043):** `config.SubagentsEnabled`
  and `tasks.allow_delegation` both default true; the drivers compose them as AND
  and the tool is registered only when the composed gate is on (`Execute` /
  `RunInteractiveTurn`). Either kill switch removes the tool from the roster
  entirely. Tests: `TestExecute_RegistersSpawnToolOnlyWhenEnabled`,
  `TestRunInteractiveTurn_SpawnToolRegistration`,
  `TestSubagents_KillSwitchesHideTool` (fake-LLM e2e).

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

## #1043 amendment — default-on, parent decides, typed children

Off-by-default and "never interactive" were the right ship-the-engine posture and
the wrong finished-product posture: operators had to discover a hidden flag before
a job could fan out, while the agent — which sees the work and already holds every
spawn argument — was the party positioned to decide. Industry harnesses (Claude
Code, Codex, Grok Build, OpenCode, Goose) register the delegation tool by default
and let the parent decide; #1043 aligns fleet. The behavioural deltas, none of
which move a wall:

- **Enablement inverts from `env || task` (both default false) to `env && task`
  (both default true).** The operator only ever opts *out*: per task via
  `allow_delegation: false`, or fleet-wide via `FLEET_SUBAGENTS_ENABLED=false` /
  Admin → Features. Existing task rows are **backfilled to true** (migration 061;
  pre-#1043 false was just the default nobody chose, not an explicit opt-out —
  recorded as a behavior change in the CHANGELOG). `TaskCreate.allow_delegation`
  becomes tri-state (`*bool`): an omitted field — old exports, bundle templates,
  API clients — means the default (true), an explicit false survives every
  round-trip.
- **Do not force fan-out.** Default-on means the *tool is registered*; the parent
  decides. The prompt section + tool description carry the policy (spawn for
  independent parallel work; don't for sequential steps; prefer `explore`; omit
  budget args unless slicing smaller). A sequential parent that never spawns is
  pinned as a passing e2e (`TestSubagents_SequentialParentNeverSpawns`).
- **Interactive chat is in scope.** The old "never interactive" rule guarded
  against unattended cost — and default-on turns unattended delegation on anyway,
  while chat has a human on the loop. `RunInteractiveTurn` registers the same
  tool (same walls) bound to the turn's `InteractivePolicy` for budget slicing
  and charge-back, so the chat cost chip includes child spend. A chat child runs
  the scheduled (run-to-completion) loop through the one governed core.
- **Typed children.** `role=explore` (the default — and the fallback for any
  invalid role) is a read-only research child: a single unit-tested denylist
  (`exploreDeniedNativeTools`) strips write-capable native tools from its FINAL
  composed roster; MCP write tools are not name-inferred (out of scope) — the
  child's prompt carries the read-only instruction. `role=worker` keeps the full
  scheduled roster. This is what makes default-on safe: the common research case
  spawns a child that structurally cannot write. One tool, role as an argument —
  never a second tool name.
- **Child write isolation.** Every child (either role) gets a unique
  `<workspace>/subagents/<child-session-id>/` directory forced as its bash/file
  default cwd — still inside the parent's sandbox and bind-mounted workspace, so
  privilege is unchanged; only default write paths de-conflict. The JSON result
  gains `{role, child_session_id, workdir}` and the parent-log `subagent_spawned`
  linkage entry gains role + workdir, which the task page / chat render as child
  cards (id, role, status, spend).

Walls that did **not** move: one governed core, monotonic privilege, the 10%
remaining-budget fraction with refuse-over-cap and atomic reserve+settle, depth 1,
fan-out 5, credentials host-side.

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
- Since #1043 the tool is present by default (a deliberate behavior change,
  CHANGELOG'd); a default deployment's runs may delegate, bounded by the same
  parent ceiling. The operator's control is the two kill switches, not discovery
  of an enable flag.
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
