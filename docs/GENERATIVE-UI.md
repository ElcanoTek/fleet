# Generative UI: interactive cards the agent builds in chat

`show_ui` lets the interactive chat agent answer with a purpose-built card
instead of prose: a form, a picker over options it fetched from an MCP server,
a multi-item editor, a pass/fail checklist, a proposed-changes diff, a chart or
a small calculator. The card is drawn inline in the transcript. Its buttons send
the user's answers back as their next message. It is fleet's take on the
"generative UI" pattern that ChatGPT shipped as *Intelligent UI* (Oct 2026):
the model picks from a library of renderable components rather than writing a
fixed interface.

The motivating workload is the large, AI-assisted intake form, such as a
multi-SSP deal builder. A form like that is hand-built per product and per
client, and it drifts from the tools it feeds. With `show_ui`, the agent builds
the form from what it can see this turn: the connected MCP servers, the options
it fetched from them, and the user's request. It then acts on the submission
with those same tools. The design is general-purpose. Nothing in fleet knows
about deals.

The *why* behind the security shape is
[ADR-0080](adr/0080-generative-ui-cards-are-declarative-data.md).

## How it works

```
model ──show_ui{title, components, actions}──▶ genui.Validate (Go)
                                                 │ invalid → tool error naming each path; model fixes, re-calls
                                                 ▼ valid → "UI_DISPLAYED card_id=<call id> … end your turn"
web: tool.call event / persisted tool_call ──▶ GenerativeCard renders the spec
user presses a submit button ──▶ an ordinary user turn:
    [UI submission] card=<call id> action=<action id>
    ```json
    {"advertiser":"Acme","lines":[{"channel":"CTV","cpm":22}]}
    ```
model reads the values ──▶ calls MCP tools as usual (approval cards apply)
                         └▶ or show_ui again with replaces=<card id> + field_errors
```

- **No staging row and no new SSE event.** The card's spec is the `show_ui`
  tool call's own input. Every client already receives it as `tool.call`, and
  the transcript persists it, so a reload redraws the card from history. The
  submission is a user message. "Replaced" is a later card's `replaces`. The
  web derives all three from the messages (`web/src/app/chat/ui/genui/transcript.ts`),
  so a reload and a second tab agree on every card's state.
- **Interactive turns only.** `show_ui` is in `interactiveOnlyToolNames`, so
  scheduled runs, Operations Center tasks and sub-agents are never offered it.
  A headless run has no one to submit a form, and its reply would never come.
- **Submissions are user turns.** They queue like any message while a turn is
  running (`mode: "queue"`). Because the model reads the values as user input
  and then calls tools, every action a card leads to passes the one governed
  loop (`agentcore.Run`) and its approval cards. The card has no path that
  skips them.

## The component catalog

Pinned three ways: `internal/genui/spec.go`, the tool description in
`internal/tools/show_ui.go`, and the renderer registry in
`web/src/app/chat/ui/genui/GenerativeCard.tsx`. `internal/genui/testdata/catalog.json`
is the shared list that both test suites assert against.

| Kind | Components |
| --- | --- |
| Layout | `section` (collapsible), `columns` (2–4), `tabs` (`variant: steps` adds numbered steps and Next/Back), `divider` |
| Display | `heading`, `text` (plain or markdown), `callout`, `badges`, `stat`, `facts`, `table`, `status_list`, `progress`, `chart` (bar/line), `diff`, `code`, `link` |
| Input | `text_input`, `number`, `slider`, `select`, `choice` (segmented/radio), `multi_select` (options and/or free entry), `toggle`, `date`, `list_input` (paste one per line), `include_exclude` (allow/block lanes), `repeater` (add/duplicate/remove items) |

Plus card-level `actions` (`submit` sends the values; `message` sends a fixed
reply, which serves as quick-reply buttons), `field_errors` (inline errors on
`id` or `repeater[i].field`, with `i` the 0-based item position, unlike the
1-based `index` in expressions), and `replaces` (collapses an older card).

A quick reply is sent as `[UI reply] card=<id> action=<id>` followed by the
button's text, so the card it answered is known from the marker rather than by
matching words. Either kind of answer locks its card. While an answer is
queued behind a running turn, the card's buttons stay held, and that hold
survives the transcript scrolling the card away. A send whose response was lost
while the server could not be reached is held the same way (a resend could
duplicate it); once the connection recovers and the server shows it never
received the answer, the hold is released with "Not sent. Try again."

A `table` with `select: single|multi` is an input that submits its `row_key`
values; every row must carry a unique, non-empty string under `row_key`.
A `status_list` item with `field` gets a **Fix** link that jumps to that
field, revealing it if it sits in another tab. Inputs hidden by `visible_if`
are neither validated nor submitted, so a stale hidden value never reads as an
answer the user gave.

Conditions are checked for logic that can never work. Each `visible_if` /
`disabled_if` is reduced to its `&&` / `||` / `!` structure and every
possible state is tried: a condition that can never be true, or an action that
is hidden or disabled in every state, is refused. A toggle compared with
`true` / `false` is the toggle itself, and a number or slider input compared
with number literals (`n > 0`, `n >= 0`, a bare `n`) is tried at concrete
values covering every range those literals separate, blank included (a slider
only at positions its range and step allow; for an action that validates, an
always-shown number input only at values its own `min` / `max` / `step` /
`required` accept, and an always-shown required toggle only on, since
nothing else can be submitted). Anything
else (arithmetic, two inputs compared, text and lists) is treated as unknown,
so the check never refuses a card that could work (`internal/genui/satisfy.go`).

A disabled input is submitted without being validated (the user cannot fix
it), so the validator refuses any default the browser would reject as an
answer: a required field left blank, a value off its length, format, range,
step or item bounds. This is a pinned pair too: the validator and the
browser's `checkField` run the same cases from
`internal/genui/testdata/disabled_defaults.json`.

Every valid card in `cards.json` is also rendered in a structural
accessibility test: each control, group and table has an accessible name,
every `aria-labelledby` / `aria-describedby` reference resolves, and ids are
unique, both as first shown and after a submit shows its errors.

## Expressions

The one logic surface is a small, side-effect-free expression language. It is
used in `{{ }}` templates (titles, text, stat values, item labels) and in
`visible_if`, `disabled_if` and `progress.value`. It supports literals, input ids
(in a repeater, the item's own fields and `index`; from outside, `rep.field` as
an array), `+ - * / % == != < <= > >= && || ! ?:`, member access, and a fixed
function whitelist: `len count sum avg min max abs floor ceil round fixed number
string upper lower join contains empty unique`.

It is a pinned pair. `internal/genui/expr.go` parses and checks every name it
reads against the card's inputs, so a typo becomes a tool error the model fixes
in the same turn. `web/src/app/chat/ui/genui/expr.ts` is a hand-written
interpreter over the same grammar (never `eval`) that evaluates forgivingly:
missing names are `null`, `null` is 0 in arithmetic, and division by zero is
`null`. No result is ever a non-finite number: an overflow or `NaN` from any
operator or function becomes `null`. Both run `internal/genui/testdata/expressions.json`.

## Limits

These are protocol constants in `internal/genui/spec.go`, not operator knobs:

- 128 KiB per card (the size the agent loop replays to the model verbatim,
  `agentcore.HardMaxToolOutputBytes`), 800 components, 12 levels of nesting
- 2,000 options per input, and 2,000 chosen entries per `multi_select` /
  `include_exclude` (custom entries included); 500 table rows, 20 columns;
  500 entries per `badges` / `facts` / `status_list` / `diff` list, and
  2,000 table rows, list entries and repeater item fields (each starting
  item times its fields, a table or list among them counted per item too)
  across the whole card
- 8 chart series of 200 points, and 3,200 points × series across the card;
  6 actions
- 200 repeater items, 20,000 `list_input` lines, and 20,000 collection-default
  entries across a repeater's starting items (each item copies its fields' defaults)
- 500-character expressions, and 2,000 characters of `{{ }}` output per
  displayed template (longer results are clipped with "…"; conditions see
  them whole); `min_length` at most 20,000, and the required
  fields' `min_length` and shortest required option (all of them, shown or
  not), plus every input's key and quoting, must add up to an answer that can
  be sent; anything else that makes an answer too large is caught in the
  browser, which says "Too large to send" instead of sending

One limit is the web card's, not the spec's: a single answer may be at most
256 KiB once JSON-escaped (`MAX_SUBMISSION_BYTES` in `genui/model.ts`, about
65,000 tokens). The answer is one user turn the agent loop cannot shrink, so
it has to leave room in the model's context window, not just fit
`/api/chat`'s 1 MiB request body. 20,000 long `list_input` lines can pass
that; the card then says the answer is too large before sending anything.

A card from before the conversation's summary (visible only when the user
expands compacted history) renders locked, with a note to ask for it again:
the model no longer has that card's definition in context, so an answer to it
would arrive as ids it cannot read.

## Using it for deal creation (and forms like it)

A bundle can teach the pattern with a skill or protocol. Nothing in fleet needs
to change. The loop that replaces a bespoke form:

1. The user describes the request ("set up deals for the Acme Q4 campaign").
2. The agent fetches what the form needs from the connected MCP servers:
   seats, catalogs, segments, list libraries.
3. It shows one card. Fetched values become `select` / `multi_select` options.
   Per-item settings become a `repeater`. Allow/block targeting becomes
   `include_exclude`. Domain lists become `list_input`. A live `stat` shows the
   resulting deal count. There is a **Check** button and a **Create** button
   with `confirm`.
4. On **Check**, the agent validates the values against the live systems. It
   re-shows the card with `replaces`, the values prefilled, `field_errors` on
   what failed, and a `status_list` audit with **Fix** links.
5. On **Create**, it calls the SSP tools. Their critical-tool approval cards
   gate each write exactly as before, then it reports per-deal outcomes, for
   example as a `table`.

## Honest scope: what this does not do

- **No progressive rendering.** The card appears when the tool call is
  complete and validated. Fleet streams tool-call inputs as one event, not
  token by token. In practice validation is instant, but a large card appears
  all at once rather than assembling as it streams, which ChatGPT's version
  does.
- **No browser-side tool calls.** A select cannot query an MCP server as the
  user types, and a button cannot call a tool directly. Options are fetched by
  the agent before the card is shown. This is deliberate (ADR-0080): it keeps
  every action inside the governed turn and keeps credentials host-side.
- **No file upload component.** `list_input` takes pasted lists. A file the
  user wants to supply goes through the composer's attachments as before.
- **Web chat only.** The terminal client (`fleet chat`) and ACP editors see the
  tool call. The TUI prints a line pointing to the web chat, and the user can
  reply in text. ACP clients get no such pointer: they show the raw tool call. Shared and read-only transcripts carry text only, so they show
  the submitted-answers bubble but not the card.
- **The card leaves the model's context like any other tool call.** A card
  fits the size the agent loop replays verbatim, but when a long conversation
  forces context reduction, an old card's definition can be summarized away
  like any other tool payload while the card stays live in the browser. Its
  answer still names the card and keys every value by field id; the model
  sees the labels again only if it re-shows the card.
- **No per-deployment switch.** The tool is part of the interactive roster for
  every deployment. An operator who wants it off has no setting yet.
- **Drafts are per browser, and bounded.** Unsent answers persist in
  `localStorage` (they survive the virtualized transcript's remounts and
  reloads), not across devices. A draft lasts 7 days, and only the 20 most
  recently edited cards keep one: an abandoned card has no other cleanup. When
  the browser's storage is full, older drafts are dropped to make room. An
  **Edit and resend** draft survives a remount too, reopening the card for
  editing until the resend lands. A restored answer (from the transcript or
  a draft) never puts a value the user cannot fix into a disabled input: a
  disabled input keeps the card's default (in a repeater item, a value the
  card itself set), a slider keeps only a value its range can hold, and
  restored repeater items stay within the card-wide row budget.

A card send goes through the composer's send path, but it leaves the composer
alone: the text being typed stays, its pending attachments are not sent with
the card answer, and a refused card answer is not restored into it.
