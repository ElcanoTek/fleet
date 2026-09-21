# Commitment transport alias

## What shipped

A scheduled `confirm_audit` binds each typed `critical_actions` entry to a full
server-qualified tool name. Finish is refused until every declared action
succeeds. On 2026-09-21, task `<task>` committed to
`mcp_pages_update_page_data`, had the inline call rejected (payload too large),
then published the same data through `mcp_pages_update_page_data_upload`
(live version for slug `<slug>`). Enforcement still demanded the inline name, and the only
exit was a self-audit abort recorded as a run ERROR.

This change treats a small, bundle-gated set of **same-write, different-transport**
pairs as one obligation:

- `update_page_data` / `update_page_data_upload`
- `deploy_page` / `deploy_page_upload`

A pair is enabled only when the installed `critical_tools` list already contains
**both** names (the bundle opted both into the gate). Extra pairs are declared
only in `agent_policy.critical_tool_transport_aliases`. One declaration is an
equivalence class (`foo: [foo_upload, foo_file]` makes every pair mutually
reachable). `critical_tool_substitutes` is a different contract and is never
treated as a transport alias.

Identity for alias discharge is the **record-id arguments** the bundle already
uses for commitments (`deal_id` / `deal_ids`, plus `slug` and any
`critical_tool_identity_keys` on the *call*). The confirm_audit `identifier`
field stays log-only. A successful alias call discharges only when its record
set is a subset of the committed set; a partial batch resumes the same way as
the inline path. An unbound commitment (no record ids) is one tool-level
obligation, which is the original pages incident.

When one `confirm_audit` envelope lists both transport names for the same
record set, they coalesce to **one** obligation.

The alias does **not** change which tools are critical.

## Deviations

None from the incident fix. Codex asked not to infer every `_upload` suffix
and not to make the pages pairs unconditional engine policy; both are honored.

## Deliberately not handled

These still require a re-audit (or a later, narrower change):

- Two unbound writes to different pages in one envelope, distinguished only by
  the log-only `identifier` or by call-side `slug` that the audit entry does
  not carry as `deal_id` / `deal_ids`. The engine cannot tell them apart
  without using `identifier` as authorization identity, which it does not.
- Different value digests on the two transports of the same record set in one
  envelope. They coalesce to one obligation; a digest-bound sibling is not
  kept.
- Treating `critical_tool_substitutes` as transport aliases. Substitutes keep
  their existing discharge rules.
- Inferring every `_upload` suffix as an alias. Only declared families match.

## Deferred

Bundles that already list both names in `critical_tools` get the pages pairs
without a YAML change. Bundles that want additional pairs must set
`agent_policy.critical_tool_transport_aliases`; no other pairs are inferred.
