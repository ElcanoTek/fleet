# ADR-0085: A bundle may hide MCP tools from interactive chat only

- **Status:** Accepted
- **Date:** 2026-10-20
- **Deciders:** fleet maintainers
- **Relates to:** [ADR-0042](0042-child-side-mcp-scope-authorization.md)
  (child-side MCP scope authorization), [ADR-0007](0007-governed-sub-agents.md)
  (governed sub-agents), [ADR-0082](0082-approval-card-describers-run-a-declared-read-at-staging.md)
  (approval-card describers), the "Governance is one core" invariant in
  `AGENTS.md`.
- **Design note:** [`INTERACTIVE-HIDDEN-TOOLS.md`](../INTERACTIVE-HIDDEN-TOOLS.md)

## Context

Until now, every gate that removes an MCP tool from a run applied to chat and
scheduled runs alike, except the persona policy, which follows the persona
rather than the kind of run. A bundle's server allowlist (Gate-2) is
deployment-wide.

Deal-booking bundles need a split. A scheduled protocol uses raw write tools
directly. Chat must reach the same writes only through a reviewed flow that
stages one approval card for a whole plan. A real chat log showed the model
calling a raw write through the generic `tool_call` bridge, which produced
one approval card per deal. Prompt instructions alone did not prevent it.

Hiding a tool from chat is a new difference between the two kinds of run. The
trunk is meant to stay free of mode branches (`TestSeamPurity`), and the
governance has to stay in one loop.

## Decision

A bundle may list tool-name suffixes in
`agent_policy.interactive_hidden_tools`. Matching is the `critical_tools`
suffix rule, applied to MCP tool names only.

1. **Driver data, not a mode branch.** The interactive driver puts the list on
   each chat turn's `RunConfig.HiddenToolSuffixes`. Scheduled drivers never
   set it. `agentcore.Run` applies whatever list it is given and does not
   inspect the mode.
2. **A narrowing only.** It removes tools that the earlier gates registered and
   never adds one, like the persona policy. It stays parent-side, as
   ADR-0042 keeps persona narrowing. The credential-owning child still
   authorizes every call against its own bundle.
3. **Uncallable, not just unlisted.** The tool is dropped before the
   disclosure decision, so it is in neither the tool list nor the deferred
   registry. `mcpTool.Run` refuses a hidden name before the policy gate, so no
   card is staged and nothing is journaled or dispatched. `tool_describe`,
   `tool_call` and the out-of-roster recovery all answer with a fixed
   explanation instead of "not found". It is the last gate: a tool the
   persona policy or the schema check also removes is explained by that
   gate, which holds in every mode. The bridges give a persona-denied name
   the persona explanation too, so the answer is the same at every point.
4. **Delegation inherits it.** A sub-agent spawned from a chat turn carries the
   list at every depth (monotonic privilege, ADR-0007).
5. **Host-side reads for a chat turn honor it.** The approval-card describer
   is not called when it is hidden, alongside the persona and hook checks
   ADR-0082 already requires. Email prevalidation (`validate_email_content`
   when a chat turn stages `preview_email` / `send_email`) is skipped when it
   is hidden, as when the validator is unreachable: it is best-effort, so the
   email stages unvalidated.
6. **The chat Tools picker matches the turn.** The picker's listing routes
   drop a hidden tool from each connector's tools and count, and do not list a
   connector whose every tool is hidden, so the UI never offers what the run
   gate refuses. A per-user hosted connector is the exception: its row carries
   no tool names (its catalog is known only once a turn opens it), so it is
   listed even when every tool it exposes is hidden; it names no hidden tool,
   and the run gate still drops them from the turn.
7. **Absent key, no change.** Without the key the list is nil and every run
   is built exactly as before.

## Consequences

- A bundle can steer chat to a reviewed flow without removing the raw tools
  from the scheduled runs that need them, and without fleet knowing what the
  tools do.
- Chat can still schedule a task that uses a hidden tool. That task is a
  scheduled run, governed by the audit gate rather than a chat approval card.
  The design note says so. A bundle that must prevent it gates scheduling in
  its persona or protocol.
- The list is read at the start of each turn from the policy installed at
  boot. Changing it needs a restart, like every other `agent_policy` key.
- A card staged before the key existed still runs if a person approves it.
  That needs a restart in between, and approval is a person's decision.
