# The chat view while the agent works

What the default chat view shows while a tool-using turn is in progress, what
"Show details" adds, and the step-joining fix underneath both (owner + design
review of the deal workflows, 2026-10-20).

## The problem

A tool-using turn is several model steps. Before each tool call a model
usually writes one line of narration ("Let me look up the deal first."). The
runtime streamed every step's text as `text.delta` into one string, and only
at the end did `text.replace` swap in the persisted final answer. The web
client rendered that string as the reply while the turn ran, so the default
view (details off) showed:

- the narration of every step, run together with no break between steps —
  `…the deal first.Found it. Now I'll check…`, the "no spaces with the
  periods" the review reported;
- internals the detailed view exists for;
- no working status: the "Thinking" / step-label line hides as soon as any text
  arrives, so after the first sentence a busy agent looked stalled.

## What shipped

**Steps are separated where the boundary is known (server).** `streamSink`
marks a step boundary on fantasy's `OnStepFinish`; the next step's first
`text.delta` carries a paragraph break (`agentcore.StepTextSeparator`: a
blank line, counting newlines already at the seam). It rides in the event
itself, so every consumer gets the readable string: the web, `fleet chat`,
ACP, turn-journal recovery, and the fallback answer of a provider that returns
no completed text. Blocks *within* one step are still joined raw, because a
provider may split one sentence across blocks
(`TestRunPreservesAllCompletedTextBlocks`). The leaked-tool-call retry in
`internal/agent` is a multi-step loop of its own and does the same.

**Narration is not the reply (web).** `turnNarration.ts`:

- A `tool.call` ends a step, so the text that step streamed moves from
  `content` to the client-only `narration` (`foldNarration`). `content` only
  ever holds the step now streaming.
- In the default view, a short in-progress segment (under 280 characters,
  `NARRATION_HOLD_CHARS`) is held back while the turn streams
  (`assistantDisplayContent`): it may yet turn out to be narration, and showing
  it would flash it in and out between tool calls. The status line stays up
  instead: **Thinking**, the step label (**Running shell**, a connector's
  action), and the task_tracker item when there is one. An answer outgrows the
  hold within its first second of streaming; a short one appears when the turn
  ends.
- **Show details** shows everything as it streams: the current segment, and
  the narration in a **Working notes** block under the reasoning.
- `text.replace` marks the reply final (`finalAnswer`); the narration stays
  behind "Show details". A turn that ends without one — stopped, failed, the
  round cap, a recovered outcome — keeps its whole partial transcript as the
  reply, as before (`settleNarration`). That holds on every path that marks a
  slot done, including Stop before the `turn.cancelled` frame arrives:
  `useTurnStream` runs each transcript write through `settleDone`.
- Reasoning blocks (`reasoning.start`) and persisted text/reasoning rows join
  as paragraphs too (`textSegments.ts`), so live and reloaded transcripts read
  the same.

Final answers are untouched: same text, same rendering.

## What it does not do

- **Working notes are live only.** The server persists the final answer, not
  the narration (unchanged), so after a reload "Show details" shows reasoning
  and tool chips but no working notes.
- **The hold is a length heuristic.** Narration longer than 280 characters
  still shows briefly in the default view before its tool call folds it away;
  an answer shorter than that shows when the turn ends rather than streaming.
- **ACP cannot retract streamed text**, so an editor shows the narration ahead
  of the answer, now with a paragraph between steps
  (`contracttest.Expected.AppendOnlyText`).

## Tests

- `internal/agentcore/step_text_separator_test.go` — the separator, the sink
  (chunks vs. steps, rollback), and a `Run` whose live stream fails without the
  fix with exactly `"Let me check the deal.Found it."`.
- `testdata/contracts/chat-stream/narrated-tool-loop.sse` — a recorded
  producer turn that narrates before each tool call (fakellm tool steps can now
  carry narration); every consumer replays it, and the web oracle checks
  `content` and `narration`.
- `web/src/app/chat/ui/chat-experience.narration.test.tsx` — the real chat
  view fed frame by frame: status line instead of narration, Working notes
  under Show details, the final answer, a long answer still streaming, and a
  stopped turn keeping its transcript.
- `turnNarration.test.ts`, `useTurnStream.narration.test.ts`,
  `history.test.ts` — the reducers, an older server's glued steps, a failed
  turn, reasoning blocks, persisted rows.
