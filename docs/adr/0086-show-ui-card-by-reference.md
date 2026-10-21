# ADR-0086: show_ui may show a card a tool built, by reference to that tool's result

- **Status:** Accepted
- **Date:** 2026-10-20
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0080](0080-generative-ui-cards-are-declarative-data.md)
  (decision 5, "No new state. The spec is the persisted tool-call input.")
- **Design note:** [`GENERATIVE-UI.md`](../GENERATIVE-UI.md#a-card-a-tool-built-from_result)

## Context

Bundles now build `show_ui` specs in code. A deal-plan server's `intake_card`
returns `{"show_ui": {...}}`, 3–13 KB, and the model has to copy it verbatim
into `show_ui`. That costs output tokens on every booking, and it is fragile.
In a real log the model copied the spec correctly, decided it had built the
card itself, redrew an identical card with `replaces`, and described its
internals to the trader.

ADR-0080 made the spec the `show_ui` call's own input. That is what lets a
reload or a second tab redraw a card with no staging row and no new event. A
reference changes where the spec comes from, so it needs its own rules.

## Decision

`show_ui` accepts `from_result: {call_id | tool, path?}` in place of an
inline spec, with only `replaces` and `field_errors` allowed beside it.

1. **Only results the model can see in this conversation.** The run gives
   `show_ui` a read-only view of the tool results the turn holds: its replayed
   history and the run's own results so far. It opens no scope, calls no tool,
   and reads no other conversation. The referenced result must be a success
   that is not an error, and it must be the newest match. An older success
   never stands in for a newer failure.
2. **Validated exactly like an inline card.** The object at `path`, with the
   allowed overrides laid over it, goes through `genui.Validate` and the
   secret-redaction check, with the same limits. A card that carries a
   redaction marker is refused. A refusal is a `UI_INVALID` tool error, as
   before.
3. **The spec is display data on the `show_ui` result.** On success the
   validated spec is attached to that result's transcript entry and
   `tool.result` event as `card` (JSON text). It is persisted with the row, so
   reload and second tabs still work. Replay never sends it to the model. The
   model sees the same short acknowledgement as for an inline card. This adds
   no table, no migration and no new event.
4. **Nothing else about ADR-0080 changes.** The card is still declarative data
   over the fixed catalog, rendered by fleet's components. Its submissions are
   still user turns, and it is still interactive-only.

## Consequences

- A tool-built card costs the model a few dozen output tokens instead of
  kilobytes, and it is shown exactly as the bundle built it. The tool
  description tells models to prefer the reference and not to rebuild or
  narrate the card.
- A tool result, which is vendor or bundle output, can now become a card
  without the model re-emitting it. That is not a new trust level. The model
  already chose to show it and could have copied it, and the card is
  validated and rendered as inert text either way.
- The spec is stored twice: in the referenced result and on the `show_ui`
  row. That is bounded by the 128 KiB card limit.
- The durable turn journal does not record `card`. A turn rebuilt from the
  journal after a crash draws no referenced card, and the model can show it
  again. The design note records this.
