# ADR-0034: Audit-gate commitment binding, payload-level failure, and create reconciliation

Status: accepted

## Context

The scheduled-mode audit gate (ADR-0001's single governed loop, `internal/agentcore`)
accepted the typed `critical_actions` field of `confirm_audit` but collapsed every
declaration to a per-suffix commitment count. Three gaps followed, all fixed in the
v1 engine after real incidents and ported here before v1 retires
(#715, #716, #717):

1. **Suffix-only matching.** One approval on a critical suffix could authorize —
   and be silently discharged by — a same-suffix tool on a *different* MCP server
   or client variant (the wrong seat), and batch calls carried no record-id or
   value-set binding at all.
2. **Payload-blind success.** Tool success was `err == nil && !resp.IsError`. Many
   MCP servers report application failures in the payload over a clean transport
   (`{"success": false}`, a top-level `"error"`); such a result discharged the
   commitment and let the run finish claiming the write happened. Only
   `send_email` had a payload check.
3. **No start-of-run create reconciliation.** The bundle servers' cross-run
   create ledger records a fail-closed pre-POST marker before every
   non-idempotent create, but fleet never replayed unresolved markers into a
   retried run's prompt, so a resume could blindly re-create records.

## Decision

**Typed commitments bind to the full tool name, record ids, and values digest.**
A typed `critical_actions` entry must name the full server-qualified MCP tool;
bare suffixes and paraphrases are dropped fail-closed. A typed audit that
resolves to zero commitments authorizes nothing: when any entry named (or
paraphrased) a known critical suffix the audit is refused outright so the agent
corrects the tool name, and an explicit no-op declaration (e.g. `"none"`, the
shape the evidence schema forces on runs with no critical work) is accepted for
completion while the engaged-but-empty binding gate still blocks every critical
call. Neither shape degrades to the old one-shot bearer token. While a typed audit is active, every
critical call must match an outstanding commitment (exact tool name, or a
bundle-declared substitute on the same server), single-record calls must match
the committed `deal_id`, and `deal_ids` batch calls are allowed only over the
audit-approved id set with the audit-approved `values_sha256`. Discharge follows
the same binding: a wrong-server/variant/record success discharges nothing. The
JSON field names (`deal_id`/`deal_ids`/`values_digest`/`values_sha256`) are kept
verbatim — they are the wire contract the client bundles' protocols emit; fleet
treats the values as opaque record identifiers. Untyped (legacy free-text)
audits keep the previous suffix-scoped semantics as a fallback, except that a
server-side batch always requires a typed approval.

**Payload-level failure never discharges a commitment.** Before any commitment
is marked executed, the result payload is checked for the MCP failure
convention (top-level `"success": false`, or a non-empty top-level `"error"`),
generalizing the previous `send_email`-only check to every critical tool. Batch
tools that return per-record `results[]` are discharged per succeeded record,
idempotently across resumes, and never for record ids the audit did not
approve.

**Unresolved create markers are replayed into the task prompt.** At scheduled-run
start, if the run's resolved MCP workspace dir contains a `creates.jsonl` ledger
with unresolved pre-POST markers, the byte-compatible v1 reconciliation block is
appended to the task prompt (the user portion — the cached system prefix stays
byte-stable per the prompt-cache contract), instructing the agent to verify each
listed create with read tools before any new create and to fail closed when
absence cannot be proven.

## Consequences

Money-moving writes are bound to the exact server, variant, record, and value
set a passing audit named; a drifted or prompt-injected agent can no longer
spend an approval on the wrong seat, and an upstream 4xx flattened into a clean
MCP response no longer counts as a completed obligation. Legacy free-text
audits remain accepted (unlike the v1 engine, which now refuses them) so
existing bundles keep working; they cannot approve batches. The reconciliation
replay is only as granular as the workspace directory: dedicated per-run
clients get a fresh dir each run today, so the replay chiefly protects
shared-client deployments until #707 lands per-task-stable workspaces.
