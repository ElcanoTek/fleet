# ADR-0082: Approval-card describers run a bundle-declared read at staging

- **Status:** Accepted
- **Date:** 2026-10-09
- **Deciders:** fleet maintainers
- **Relates to:** [ADR-0042](0042-child-side-mcp-scope-authorization.md)
  (child-side MCP scope authorization), the "Governance is one core"
  invariant in `AGENTS.md`, [ADR-0080](0080-generative-ui-cards-are-declarative-data.md)
  (cards are declarative data).
- **Design note:** [`APPROVAL-CARD-DESCRIBERS.md`](../APPROVAL-CARD-DESCRIBERS.md)

## Context

A critical tool's approval card shows its arguments verbatim, because fleet
cannot know what they mean. A reviewer of a record write needs the record's
name, its current values and its state, which only the bundle's server can
read. Getting them means calling an MCP tool while a call is being staged,
outside the model's turn of `agentcore.Run`. The governed loop is meant to be
the only place tools run on the model's behalf, so a call made elsewhere has
to be shown not to be a second, weaker governance path.

Fleet already makes one such call: the email stager calls
`validate_email_content` on the sending server before staging a send. This ADR
names the rules any staging-time read must follow.

## Decision

A bundle may map a critical suffix to a "describer" tool on the same server
(`agent_policy.critical_tool_card_describers`). When a matching call is
staged, fleet calls the describer once, under these constraints:

1. **Only a declared read.** The describer must be parallel-safe and must not
   be critical. Boot drops an entry that breaks either rule, validate-config
   reports it, and the stager re-checks both on the resolved tool name before
   every call. Fleet never calls a critical tool outside the approval path.
2. **The turn's own scope.** The call goes through the broker and seat the
   turn already holds, so the credential-owning child authorizes it against
   its own bundle exactly as it would a model call (ADR-0042). No new broker
   path exists.
3. **The loop's per-call gates.** A describer the turn's persona would not
   offer (Gate-4) is not called, and neither is one any `pre_tool_use` hook
   matches: the hook cannot run outside the loop, so the call is skipped
   rather than made without it.
4. **The model's own arguments, nothing more.** The describer receives the
   staged call's arguments unchanged. Fleet adds no credential, account or
   context.
5. **Bounded and best-effort.** Five seconds, both as a deadline and as the
   per-call budget the child applies, and no retries. Any failure leaves the
   generic card. Staging never waits longer and never fails because of it.
6. **Display data only.** The output never reaches the model, the transcript
   or the tool result. It is validated against a fixed text-only schema,
   refused if the secret redaction would alter it, stored on the approval row
   and rendered as text. It never changes what executes: approval still runs
   the frozen arguments.

## Consequences

- A bundle gets a readable card without any fleet code that knows its domain.
  Fleet enforces only the schema.
- Staging can take up to five seconds longer for a described tool. Undescribed
  tools are unaffected.
- The read happens at staging, so the card can be stale by the time a person
  approves. Concurrency protection belongs to the write tool (an ETag or
  equivalent check), not to the card. The design note says so.
- A describer is trusted to be a read because the bundle declared it
  parallel-safe. Fleet cannot verify that a server's tool has no side effects.
  The declaration is the same one that already lets the loop run it
  concurrently.
