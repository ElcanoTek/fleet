# ADR-0071: Bundle-declared critical tool aliases are one commitment

- **Status:** Accepted
- **Date:** 2026-09-22
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0034](0034-audit-gate-commitment-binding.md). A commitment
  may now also be matched by a bundle-declared **alias** on the same server,
  alongside the exact tool name and a bundle-declared substitute.
- **Design note:** [`CRITICAL-TOOL-ALIASES.md`](../CRITICAL-TOOL-ALIASES.md)
  (what shipped, deviations from #1604, what was deferred).

## Context

ADR-0034 binds a typed `confirm_audit` declaration to the exact
server-qualified tool name. A bundle can loosen that binding in exactly one way:
a `critical_tool_substitutes` entry on the same server. A substitute is
one-directional — "this other action may stand in for the committed one".

Pages exposes one write under two names: `update_page_data` (inline `data`) and
`update_page_data_upload` (a staged file). `deploy_page` and
`deploy_page_upload` are the same kind of pair. The agent picks the transport
from the payload size, which it only knows after it has built the payload,
often after the audit. So a run declared one name and published through the
other. The published call was either blocked, or it left the declared name owed
after the write. Production runs published correctly and then ended as
failures:

- A Pages data refresh for page A declared `mcp_pages_update_page_data` and
  published through `mcp_pages_update_page_data_upload`. The run's self-audit
  then "aborted the stale mcp_pages_update_page_data commitment because the
  exact audited payload was already successfully published through
  mcp_pages_update_page_data_upload" → status `error`, data live. A refresh of
  a second page failed the same way.
- A deploy of page B left a stale `deploy_page_upload` declaration owed after
  the publish → status `error`.

The bundle's self-audit protocol grew a "Wrong tool variant declared?" recovery
dance for this: abort, re-audit, execute. That dance is exactly what the gate
had no way to express: "either of these two names is this one action".

## Decision

A bundle may declare equivalence classes of critical suffixes in
`agent_policy.critical_tool_aliases` (`{<suffix>: [<suffix>, …]}`). Each key
and the suffixes listed under it form one class, and entries that share a
member merge. Every member must also be in `critical_tools`. A member that is
not is logged and ignored at policy install, and an entry left with fewer than
two members is dropped. That way an alias can never discharge through a tool
the gate does not see, and a typo is loud rather than silently inert.

Within a class, two full tool names on the **same server/variant prefix** are
one critical action for the audit gate, in both directions:

- A call of one member rides an outstanding commitment declared on another.
  The authorization pre-check uses the same match.
- A successful call of one member discharges a commitment declared on another.
- A re-audit declaring one member supersedes a stale, same-shape declaration of
  another, exactly as a re-audit of the same tool does. Without this, the stale
  declaration would stay owed.
- An audited call blocked before the audit is discharged by its alias, when
  the alias wrote the same record (`deal_id` or `deal_ids` set).
- Batch approvals (`deal_ids`, `values_digest`) and the per-record discharge
  ledger are keyed by the alias class. A record set approved on one member
  therefore binds a batch sent through another, and a record discharges once.
  The class key is shared, the binding is not: the `values_digest`
  requirement is kept per declared record (an undigested batch is not refused
  over a twin's digest for other records), a batch result discharges only the
  records the invoked call named in its `deal_ids`, a digest-bound batch
  commitment discharges only under its own digest, and the discharge ledger
  dedups a record per server/variant, so two servers' writes of one record id
  stay two actions. Otherwise, with one batch
  per twin, a response to the first batch reporting a record of the second
  would discharge the second commitment although its action never ran.

Nothing else about ADR-0034 changes:

- Record binding (`deal_id`, `deal_ids`, `values_digest`) carries over to the
  alias unchanged.
- A same-suffix or aliased call on a different server or client variant is
  still refused and discharges nothing.
- Approval modes (`critical_tool_modes`) stay per suffix.
- A bundle without the key behaves as before for single-record calls and a
  single batch. The batch-ledger corrections above apply to every bundle.

Untyped (legacy free-text) audits honour aliases the way they already honour
substitutes: suffix-level, because a free-text declaration carries no server
identity.

## Enforcement

- `internal/agentcore/agent_policy.go`: `buildCriticalAliasClasses` (validation
  and class building).
- `internal/agentcore/audit.go`: `criticalAliasesEquivalent`,
  `criticalAliasClassOf`, `criticalSuffixCovers`.
- `internal/agentcore/audit_commitment.go`: `typedCommitment.nameMatches`,
  `sameAliasedTool`, re-audit superseding, and batch binding keys.
- `internal/agentcore/orchestration.go`: `markPendingCriticalDone` and the
  batch discharge ledger.
- Tests: `internal/agentcore/audit_alias_test.go` covers both directions,
  cross-server and cross-variant refusal, the absent-key baseline, record and
  batch binding, re-audit, pending, legacy, and validation.
  `internal/agent/scheduled_alias_test.go` covers the prod shape end to end
  through the scheduled driver.
- The ADR-0034 tests pass unchanged.

## Consequences

A bundle can end the wrong-variant failures on deploy, for every saved prompt,
without an MCP API change, and it can drop its recovery dance. The cost is one
more place the binding can be widened, so an alias is only right for names
that are genuinely **one action** with the same blast radius and the same
record semantics. A lower-level fallback that achieves the same result by a
different action is still a substitute, not an alias. Declaring both members
of a class in one audit still registers two commitments. Unbound commitments
carry no identity that could tell "one write, declared twice" from "two
writes", so a run should declare the variant it expects to use once. The
manifest is strictly decoded, so a bundle adopts the key only after the fleet
release that understands it has shipped.

## Alternatives considered

- **Collapse Pages to one tool** (`update_page_data` accepting `data` or
  `upload_id`). This is cleaner long-term. But it needs a Pages API change, a
  manifest change and every saved prompt regenerated before the failures stop.
- **Declare two substitutes, one per direction.** The typed matcher already
  honours same-server substitutes, so this would cover authorize and discharge.
  It would not cover the other paths: re-audit superseding stays exact-name
  (the page B shape), a blocked pending call stays exact-name, and batch
  approvals stay per suffix. It would also make "one action, two names" read
  like "a fallback action" in the manifest.
