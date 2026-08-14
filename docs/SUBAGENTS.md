# Sub-agents: default-on, parent decides, typed children (#1043)

Design note for the #1043 finish of governed sub-agents (engine: #175/#264,
budget settle: #588, ADR: [0007](adr/0007-governed-sub-agents.md), amended in the
same PR as this note). For the runtime mechanics (budget slicing, caps,
parallelism) see [AGENT-RUNTIME.md](AGENT-RUNTIME.md#governed-sub-agents--agent-delegation-175-264-finished-by-1043).

## Product decision

**Sub-agents are on by default. The parent agent decides whether to use them.**
The operator only ever opts out — per task, or fleet-wide. Registering the tool
is the feature: a sequential run that never calls `spawn_subagent` is a
successful use of it. Fan-out is never forced.

## What shipped

- **Enablement inverted** from `FLEET_SUBAGENTS_ENABLED || task.allow_delegation`
  (both default false) to `FLEET_SUBAGENTS_ENABLED && task.allow_delegation`
  (both default **true**). Two independent kill switches: Admin → Features
  `subagents_enabled` / `FLEET_SUBAGENTS_ENABLED=false`, and per-task
  `allow_delegation: false`. When the composed gate is off the tool is **not
  registered** (structural, not a soft check).
- **Backfill.** Migration 061 flips the column default to true and backfills
  existing rows (pre-#1043 `false` was the default nobody chose, not an explicit
  opt-out). **Behavior change:** existing scheduled tasks start seeing
  `spawn_subagent` — recorded in the CHANGELOG.
- **Tri-state create.** `TaskCreate.allow_delegation` is a `*bool`
  (`DelegationAllowed()` resolves nil → true), so an old export, bundle template,
  or API client that omits the field gets the default while an explicit false
  survives create/edit/clone/rerun/recurrence/export/import round-trips. The
  task's stored column and API `Task.allow_delegation` always serialize
  explicitly (no `omitempty`) now that false is the non-default.
- **Interactive chat in scope.** `RunInteractiveTurn` registers the same tool
  when the fleet flag is on (chat has no per-conversation column — the fleet
  toggle is the only chat opt-out). The tool binds to a host `*Agent` carrying
  the turn's wiring and to the turn's `InteractivePolicy` for budget slicing +
  charge-back, so the chat cost chip includes child spend. A chat child runs the
  scheduled (run-to-completion) loop through the one governed core — same walls.
- **Typed children.** `role` argument on the one tool (never a second tool
  name): `explore` (default; any invalid value falls back to it) strips
  write-capable native tools via the single unit-tested denylist
  `exploreDeniedNativeTools` applied to the child's **final composed roster**;
  `worker` keeps the full scheduled roster. Both roles drop the
  interactive-only staging tools.
- **Child write isolation.** Every child gets
  `<parent workspace>/subagents/<child-session-id>/` created before it runs and
  forced as its bash/file-tool default cwd (the same `WithForcedWorkingDir` seam
  worktree isolation uses; the forced dir now takes precedence over the
  per-conversation workspace on every tool surface so a chat child's writes land
  in its subdir). Still the parent's sandbox — no new namespace, no privilege
  change. A dir that cannot be created refuses the spawn rather than running
  unisolated.
- **Richer results + visibility.** The tool returns
  `{result, cost_usd, tokens, success, role, child_session_id, workdir}`; the
  parent log's `subagent_spawned` linkage entry carries role/workdir/spend. The
  task page (stored transcript + live stream) and the chat transcript render
  child cards — id, role, status (running/done/refused/timed out/failed), spend,
  workdir, result behind a disclosure — never raw JSON blobs.
- **Parent policy as prompt.** When the tool is registered, a short delegation
  section is appended to the system prompt (scheduled + interactive): spawn for
  independent parallel work; don't for sequential steps or one cheap call;
  prefer `explore`; omit budget args unless deliberately slicing smaller; the
  child sees nothing of the conversation. The tool description carries the same
  rules. No swarm-config UI — the create form has exactly one opt-out toggle.

## Walls that did not move

One governed core (`agentcore.Run`); monotonic privilege (inherit sandbox / MCP /
creds, subtract only); hard budget split (≤10% of remaining per child, refuse
over-cap, atomic reserve+settle under the parent mutex); depth 1 (children have
no spawn tool); fan-out 5; credentials host-side.

## Deviations & deliberate deferrals

- **Explore is "no purpose-built writers", not a filesystem guarantee.** Bash
  cannot be made read-only, and MCP write tools are not name-inferred (out of
  scope per the issue). The strip set + the child's read-only prompt section is
  the honest posture.
- **Child transcript links.** The child cards show id/role/status/spend/workdir
  and the child's final answer; the child's full transcript is its sibling
  session-log **file** (`fleet-session.subagent-<id>.json`, path derived by
  `childLogFilePath`). No HTTP endpoint serves those files yet, so the cards do
  not deep-link a child transcript viewer — deferred as a follow-up rather than
  shipping a hasty file-serving route.
- **Chat children use the turn user's default MCP accounts.** Per-server
  non-default account choices are not threaded into chat children.
- **Also deferred** (out of scope per the issue): depth > 1, raising the 10%
  fraction, `subagent_start`/`subagent_end` hooks, per-conversation chat
  opt-out, operator-picked child count/model in the form, git
  worktree-per-child.

## Tests

Unit: role normalization + strip-set pinning + explore/worker rosters end to end
(`internal/agent/subagent_role_test.go`), workdir isolation + JSON shape,
interactive registration on/off, InteractivePolicy budget seam, plus the
pre-existing budget/depth/fan-out/`-race` suites (all still green).
E2E (fake-LLM, no key): `internal/taskrun/subagents_e2e_test.go` — a DEFAULT
task fans out an explore + a worker child through the full scheduled runtime
(charge-back into the parent totals, linkage entries, sibling child logs,
explore offered no `write_file`, children offered no `spawn_subagent`); a
sequential parent finishes with the tool advertised and zero spawns; both kill
switches structurally hide the tool. UI: TaskCreateModal toggle payloads and
LogViewer child-card rendering (vitest).
