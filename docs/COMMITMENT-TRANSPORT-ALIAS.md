# Commitment transport alias

## What shipped

A scheduled `confirm_audit` binds each typed `critical_actions` entry to a full
server-qualified tool name. Finish is refused until every declared action
succeeds. On 2026-09-21, task `<task>` committed to
`mcp_pages_update_page_data`, had the inline call rejected (payload too large),
then published the same data through `mcp_pages_update_page_data_upload`.
Enforcement still demanded the inline name; the only exit was a self-audit
abort recorded as a run ERROR.

**SCOPE RULE** — an alias discharge happens only when all four hold:

- **(a)** the pair is declared in `critical_tool_transport_aliases` (pages
  pairs also count when the bundle already listed both names as critical);
- **(b)** the committed inline call was **rejected by the server** (tool error,
  nothing written);
- **(c)** the alias call carries the **same identity key names** with the
  **same values** as that rejected call;
- **(d)** the record set is **identical** (no subsets).

Anything else stays pending exactly as before until the model re-audits
(fail closed). The alias does not change which tools are critical.
`critical_tool_substitutes` is a different contract.

## Deviations

None from the incident fix. Codex asked not to infer every `_upload` suffix
and not to make the pages pairs unconditional engine policy; both are honored.

## Deliberately not handled (requires re-audit)

- **Transitive overlapping groups** (`foo: [bar]` and `bar: [baz]`): each
  declaration is a direct pair only, so `foo` and `baz` stay unrelated — a
  retry that jumps the gap is blocked until the model re-audits the name it
  actually used.
- **Subset re-audit superseding**: after a batch `[A,B]` partially discharges
  `A`, re-auditing the alias for remaining `[B]` does not retire the old
  commitment (`sameDealSet` compares the original map); finish still owes `B`
  until a re-audit of the same full set, or of the exact remaining tool name.
- **Cross-key canonicalization** (`slug: "home"` vs `page_id: "home"`): key
  names are part of the identity, so different keys with the same value are
  not the same write and stay pending until re-audit.

## Deferred

Bundles that already list both names in `critical_tools` get the pages pairs
without a YAML change. Extra pairs must be listed in
`agent_policy.critical_tool_transport_aliases`; no other pairs are inferred.
