# ADR-0083: A settled approval card may start one chat turn, through the queue

- **Status:** Accepted
- **Date:** 2026-10-10
- **Deciders:** fleet maintainers
- **Relates to:** the "Governance is one core" invariant in `AGENTS.md`,
  [ADR-0080](0080-generative-ui-cards-are-declarative-data.md) (a card never
  acts; its submit is an ordinary user turn), the input queue's
  no-auto-drain-at-boot rule ([INPUT-QUEUE.md](../INPUT-QUEUE.md)).
- **Design note:** [`RESUME-AFTER-APPROVAL.md`](../RESUME-AFTER-APPROVAL.md)

## Context

Until now every interactive turn started from something a person sent: a
message, a queued follow-up, a card's submit (ADR-0080 makes even that an
ordinary user turn). An approval card's outcome is written to history outside
any turn, and the agent reads it only on the user's next message. For
multi-step work that ends each step on a card (a batch of record updates, one
card per group), the agent therefore stalls after every card: it promised to
verify and could not, because nothing ran it.

Letting fleet start a turn on its own touches three properties the project
relies on: governance happens in one loop; unattended model spend is bounded
(the reason boot recovery never auto-drains the queue); and a person can tell
their own words from text fleet wrote (ADR-0058's split of injected context).

## Decision

A bundle may list critical tools in `agent_policy.critical_tool_resume`. When
a card for such a tool is settled (approved with its outcome recorded,
declined, or timed out) and no other card of the conversation is still pending
or executing, fleet starts **one** new turn in that conversation, under these
rules:

1. **Opt-in per tool, default unchanged.** Without the key no turn ever starts
   on its own, and nothing about the approval path changes.
2. **Through the ordinary queue, so through `agentcore.Run`.** The turn is an
   input-queue row (mode `resume`) drained by the same launch path as a
   follow-up the user queued: same governance, persona, model, connectors,
   seats, concurrency cap and ceilings. It never runs beside another turn of
   the conversation, and Stop covers it.
3. **At most once per card.** The claim of the settled cards and the insert of
   the row commit in one transaction; cards settled close together coalesce
   into one turn.
4. **Bounded.** A per-conversation hourly cap (default 10, bundle-settable
   1–60) and the queue's depth cap; past either, the conversation gets a note
   and no turn.
5. **Never after a restart.** A resume that had not started when the process
   stopped is dropped at boot with a note in the conversation, consistent with
   the queue's no-auto-drain rule.
6. **Fleet's words are labelled as fleet's.** The turn's input is a fixed,
   labelled notice naming the cards and their outcomes, persisted with
   `kind = "approval_resume"` and rendered as a notice, never as the user's
   message.
7. **It approves nothing.** Any critical call the new turn makes stages a new
   card.

## Consequences

- The invariant becomes: an interactive turn starts from a person's input,
  **or** from a settled approval card of a tool the bundle opted in, through
  the queue. No other server-started chat turn exists.
- Opted-in deployments spend model tokens without a message from the user,
  bounded by the cap; every resume is an audit log line, a counter, and a
  durable queue row.
- Clients must not assume every turn has a user bubble: the web renders the
  notice row and follows the conversation after a resumable card's answer;
  the stream contract records a turn with `input_kind`.
- A restart loses a due resume (with a note) rather than risking an
  unattended or duplicate run.
