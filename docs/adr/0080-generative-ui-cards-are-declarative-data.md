# ADR-0080: Generative-UI cards are declarative data, and their actions route through the governed turn

- **Status:** Accepted
- **Date:** 2026-10-08
- **Deciders:** fleet maintainers

## Context

Bespoke AI-assisted intake forms, deal builders above all, are expensive.
Each one is hand-built per product and per client, and each drifts from the
MCP tools it ultimately feeds. Chat already reaches those tools, but it
collects inputs one question per message, which is a poor way to fill twenty
related fields or edit a list of items. Generative UI is now a mainstream
answer: the model composes an interface for this answer from a component
library, as in ChatGPT's *Intelligent UI*.

Letting a model put interactive UI inside an authenticated session raises
three questions fleet's invariants have to answer:

1. **What does the model emit?** Code (HTML/JS in a sandboxed frame) is the
   most flexible option. It is also a new execution surface in the browser,
   holding the user's session, that fleet would have to sandbox, version and
   reason about.
2. **What can the UI do?** If a button could call an MCP tool directly, every
   such call would bypass `agentcore.Run`'s policy, ceilings, audit and
   approval cards, which is a second, weaker governance path. Browser-side
   option lookups would also need credentials the broker never lets leave the
   host.
3. **Where does it run?** Scheduled and orchestrator runs have no one to
   answer a form.

## Decision

1. **A card is declarative data rendered by fleet's own components.** The
   `show_ui` tool takes a JSON tree over a fixed catalog (layout, display and
   input components; `internal/genui`). The web renders only catalog
   components, draws every string as text, allows no raw HTML or images in its
   one markdown surface, opens only `https` links with `noopener`, and fetches
   nothing on render. The only computation is a side-effect-free expression
   language with a function whitelist, implemented as a hand-written
   interpreter and never `eval`.
2. **The server validates every spec at tool-call time** (`genui.Validate`):
   catalog, props, ids, value types, option sets, expression syntax and
   references, size limits. A bad spec is a tool error naming each path, and
   the model fixes it in the same turn. This is a feedback loop, not the
   security boundary: the renderer is independently defensive.
3. **A card never acts.** A submit composes an ordinary user message
   (`[UI submission] card=<id> action=<id>` plus the JSON values) and sends it
   through the normal chat turn. The agent then acts with its usual tools, so
   every resulting write passes the one governed loop and its approval cards.
   There is no browser-side tool call and no endpoint for cards.
4. **Interactive chat only.** `show_ui` is in `interactiveOnlyToolNames`.
   Scheduled runs, Operations Center tasks and sub-agents never see it.
5. **No new state.** The spec is the persisted tool-call input. The
   submission is a user message. Replacement is a later card's `replaces`.
   There is no table, no SSE event and no migration.

## Consequences

- The model can build arbitrary forms, pickers, checklists, charts and
  calculators without fleet shipping per-product UI. Fleet stays an engine:
  what a deal form contains is decided per turn from the bundle's MCP tools,
  not by fleet code.
- Interactivity is bounded by the catalog. Anything outside it (a map, a file
  drop, a live-search select backed by an MCP call) is a catalog addition with
  a validator rule, a renderer, a tool-description line and the shared
  fixtures, not something a model can improvise.
- Options must be fetched by the agent before the card is shown, so a very
  large catalog has to be narrowed first (2,000 options per input).
- A submission costs a model turn, by design: the model must see the answers
  to act on them.
- Credentials and the sandbox boundary are untouched. The tool does no I/O; it
  is host-side validation of its own arguments.

## Alternatives considered

- **Model-generated HTML/JS in a sandboxed iframe** (the `preview_email`
  approach, applied to interactive UI). Rejected. A sandboxed frame that can
  run script still needs a message channel back to fleet to submit anything,
  and that channel becomes a programmable surface. It also cannot be
  validated before render, and it gives up a consistent look and
  accessibility.
- **Cards that call tools directly** (a button bound to an MCP tool and
  arguments). Rejected. That is a second governance path, and the approval
  card for that tool would have to be re-invented in the card.
- **A staging row plus a dedicated SSE event**, like approvals. Rejected as
  unnecessary. The tool call already carries and persists the spec, and a
  submission is naturally a user message.
