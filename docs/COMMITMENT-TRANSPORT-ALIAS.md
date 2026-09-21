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
**both** names (the bundle opted both into the gate). Extra pairs can be declared
in `agent_policy.critical_tool_transport_aliases`, with
`agent_policy.critical_tool_identity_keys` naming the JSON argument keys that
identify the write (`page_id`, `document_name`, …). Pages pairs also match on
`slug`. An unlisted `_upload` suffix does not ride or discharge a commitment.
Cross-server matching is still refused. Unbound re-audit of an alias only
supersedes a prior write that shares the same identifier.

Re-audit of one name supersedes the other on the same server and record-set.
Batch `approvedDealIDs` / digest lookup considers every alias counterpart that
covers the call; result accounting then intersects that authorizing set with
the request's `deal_ids`. A successful call also clears pending blocked-call
rows for the alias family whose record set matches (inline content vs.
workspace-file arguments are not byte-identical).

The alias does **not** change which tools are critical.

## Deviations

None from the incident fix. Codex asked not to infer every `_upload` suffix
and not to make the pages pairs unconditional engine policy; both are honored.

## Deferred

Bundles that already list both names in `critical_tools` get the pages pairs
without a YAML change. Bundles that want additional pairs must set
`agent_policy.critical_tool_transport_aliases`; no other pairs are inferred.
