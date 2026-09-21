# Commitment transport alias

## What shipped

A scheduled `confirm_audit` binds each typed `critical_actions` entry to a full
server-qualified tool name. Finish is refused until every declared action
succeeds. On 2026-09-21, task `8486611d` (ultima-elc00179-twc) committed to
`mcp_pages_update_page_data`, had the inline call rejected (payload too large),
then published the same data through `mcp_pages_update_page_data_upload`
(live version 860). Enforcement still demanded the inline name, and the only
exit was a self-audit abort recorded as a run ERROR.

This change treats a small, bundle-gated set of **same-write, different-transport**
pairs as one obligation:

- `update_page_data` / `update_page_data_upload`
- `deploy_page` / `deploy_page_upload`

A pair is enabled only when the installed `critical_tools` list already contains
**both** names (the bundle opted both into the gate). Extra pairs can be declared
in `agent_policy.critical_tool_transport_aliases`. An unlisted `_upload` suffix
does not ride or discharge a commitment. Cross-server matching is still refused.

Re-audit of one name supersedes the other on the same server and record-set.
Batch `approvedDealIDs` / digest lookup considers every alias counterpart.
A successful call also clears pending blocked-call rows for the alias family.

The alias does **not** change which tools are critical.

## Deviations

None from the incident fix. Codex asked not to infer every `_upload` suffix
and not to make the pages pairs unconditional engine policy; both are honored.

## Deferred

Elcano-config already comments that the upload tools share blast radius with
the inline writes and lists both in `critical_tools`, so the pair activates
without a bundle YAML change. Bundles that want additional pairs must set
`critical_tool_transport_aliases`; no other pairs are inferred.
