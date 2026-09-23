# Critical tool aliases (#1604)

Design note for `agent_policy.critical_tool_aliases`. The decision and its
threat reasoning are [ADR-0071](adr/0071-critical-tool-aliases.md), which
amends [ADR-0034](adr/0034-audit-gate-commitment-binding.md). The operator
reference is the "Critical tool aliases" section of
[`AGENT-RUNTIME.md`](AGENT-RUNTIME.md).

## What shipped

- **The key.** `agent_policy.critical_tool_aliases: {<suffix>: [<suffix>, …]}`
  in the bundle manifest. Each key and its list form one equivalence class of
  critical suffixes. Entries that share a member merge. A member that is not
  in `critical_tools` is logged and ignored when the policy is installed, and
  an entry left with fewer than two members is dropped.
- **One action for the audit gate.** Two members of a class on the **same
  server/variant prefix** are one commitment, in both directions. That covers
  the pre-call authorization check, discharge, re-audit superseding (a
  re-audit that switches variant retires the stale declaration instead of
  stacking on it), and the pending list (an audited call blocked before the
  audit is cleared by its alias, but only when the alias wrote the same
  record: the same `deal_id`, or the same `deal_ids` set).
- **Batches.** The batch approval and discharge ledgers are keyed by alias
  class, so a `deal_ids` / `values_digest` approval made on one member binds a
  batch sent through the other. The class shares a key, not a binding:
  - the `values_digest` requirement is kept per declared record;
  - a batch result discharges only the records that call named in its
    `deal_ids`;
  - a digest-bound batch commitment discharges only under its own digest;
  - the discharge ledger dedups a record per server/variant, so two servers'
    writes of the same record id are two actions.
- **Cross-server stays refused.** For a typed declaration, an aliased or
  same-suffix call on a different server or client variant is still blocked
  and discharges nothing. Legacy free-text audits honour aliases at suffix
  level, the way they already honour `critical_tool_substitutes`, because a
  free-text declaration carries no server identity.
- **`fleet validate-config`** reports an ignored member or a dropped entry as
  an `agent_policy=fail` check, from the same validation the boot path runs
  (`agentcore.CriticalToolAliasProblems`). See
  [`BUNDLE-PREFLIGHT.md`](BUNDLE-PREFLIGHT.md).
- **The generic bundle** (`config/default/manifest.yaml`) documents the key in
  its schema comment. It declares no aliases.

## Deviations from the issue

- The issue scoped the change to discharge in the matcher: "nothing else about
  binding changes". Discharge alone left three of the observed failures in
  place: the aliased call was blocked before it could run, a re-audit of the
  other variant stacked instead of superseding, and a pending blocked call
  stayed owed. So the alias also applies to authorization, re-audit
  superseding and the pending list, and batch ledgers are keyed by class.
- The issue's acceptance said a manifest without the key "behaves
  byte-identically". For single-record calls and for one batch per audit,
  that holds. Review of this change found batch-ledger defects that did not
  depend on aliases, and the fixes apply to every bundle:
  - Two digest-bound batches of one tool in one audit: the first batch used to
    be refused, because only the last declared digest was kept.
  - A batch result reporting a record the call did not name: that record's
    commitment used to be discharged.
  - The same record id written on two servers: the second server's write used
    to be skipped.
- A validate-config check was added, which the issue did not ask for. At boot
  an alias problem is only a log line, and a typo would silently leave the
  failure the alias was meant to end.

## Deliberately deferred

- **Collapsing Pages to one tool** (`update_page_data` accepting `data` or
  `upload_id`). It is cleaner long-term, but it needs a Pages API change and
  every saved prompt regenerated. The alias works for existing prompts on
  deploy.
- **Declaring both members in one audit** still registers two commitments.
  Unbound declarations carry no identity that could tell one write declared
  twice from two writes.
- **Approval modes** (`critical_tool_modes`) stay per suffix, not per class.
  Give every member the same mode.
- **Record matching for the pending list** uses the ids the gate knows
  (`deal_id` / `deal_ids`). Two record-less calls, for example two calls that
  name their page only by `slug`, still match on the tool names alone.
- **Pending calls the audit did not declare** stop blocking finish once the
  audit token auto-locks. That is ADR-0034's existing rule, unchanged here:
  the audit's declarations decide what is owed.
- **Not in the forced preflight floor.** A fleet checkout without the check
  would then fail every caller. A bundle that declares aliases should add
  `agent_policy` to its `gate_checks`.
- **Bundle adoption** is separate. The manifest is decoded strictly, so a
  bundle adopts the key only once a fleet release that understands it is
  deployed. After that, the bundle can remove its "Wrong tool variant
  declared?" recovery step for aliased pairs.
