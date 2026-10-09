# Operations Center log timeline

A task's record in the Operations Center shows its stored transcript. That
view used to be one card per stored message:

- Every tool call was split across two cards, the call and then its result.
- Arguments and results were raw JSON, so `run_python` code appeared as an
  escaped one-line string.
- The stdout of a python cell appeared twice, once as `output` and once as
  `stdout` inside the envelope.
- A 46 KB task prompt rendered as one markdown blob.

A 135-step scheduled run could not be read, so its failure could not be found.
The view is now a timeline.

## What shipped

- **One step per tool call.** `buildTimeline` (`web/src/app/orchestrator/logTimeline.ts`)
  pairs each assistant tool call with its result by `tool_call_id`. A result
  with no call (a truncated log) still renders as its own step. Steps are
  numbered in call order.
- **Tool families.** `toolMeta` places a tool in one of these families: python,
  shell, files, plan, audit, email, outbound send, web, pages, delegation, or
  other MCP. An MCP name is split into its server and action
  (`mcp_ses_outbound_send_email` becomes `ses_outbound` / `send_email`). Each
  family has an icon and a `--color-tool-*` token, derived from the syntax and
  status palettes so both themes are covered.
- **Readable inputs.** Python code and shell commands are syntax-highlighted.
  Python gets line numbers. Files written or viewed are highlighted by their
  extension (yaml, json, python, shell, HTML). `task_tracker` shows a checklist
  and `confirm_audit` shows a verdict. Any other argument object becomes
  labelled rows: short scalars inline, long strings and nested values as their
  own blocks.
- **Readable results.** `parseToolResult` unwraps the `run_python` and `bash`
  envelopes into terminal output, with stderr and tracebacks in the danger
  colour, the execution time, and the exit code. Other JSON is re-indented and
  highlighted, under a headline such as "1 match · 264 scanned" or
  "invalid · 1 error". A `[fleet …]` annotation appended after a JSON body is
  split off and shown as a note, so the body still parses.
- **One failure flag.** These all mark a step failed: the explicit error bit, a
  `[tool error]` prefix, `status: "error"`, `valid: false`, a non-zero exit, or
  "Audit Failed". A failed step stays visible while collapsed and shows its
  error line. For a traceback that is the last line.
- **Runtime events.** Bracket-tagged engine lines such as `[context_compacted] …`,
  `[context_checkpoint] …` and `[fatal] …` render as slim event rows that show
  their key facts. `[fatal]` renders as a red banner instead. Session-log
  messages with `message_type: "compaction_summary"` render as a collapsed
  "Compaction summary" card. The engine writes those from #1704 on. Older
  transcripts have no summary to show.
- **Prompt, answer, overview.** The task prompt is clamped behind **Show full
  prompt**, and the last assistant answer is marked **Final answer**. An
  overview strip shows one tick per step, coloured by family, with failures in
  red and compactions as gaps. Clicking a tick opens that step and scrolls to
  it.
- **Finding things.** A search box matches tool names, arguments, results and
  text. The filter chips gained **Errors** and **Runtime events**, and tool
  chips carry their family colour. Tags are per item, so a failed call does not
  pull in sibling calls from the same assistant message. **Expand all** and
  **Collapse all** open or close every step.
- **Live runs.** The live view (`LiveTaskView`) renders its tool entries with
  the same `ToolStepCard`, so a running task and its stored log look alike.
- **Task names.** An untitled task whose prompt has a top-level YAML `name:`
  line is labelled by that name (`promptName` in `taskDisplay.tsx`). This
  applies in the modal title, the Recent Tasks title line and control labels.
  Many scheduled prompts open with the same preamble heading, so their first
  lines were identical.

## Deviations and limits

- Durations come from the session log's whole-second timestamps. A step shows
  a duration only when it took at least one second.
- The highlighter stays the lazily loaded `CodeHighlight` chunk. It gained
  optional size, line-number and wrap props plus the `markup` grammar, and chat
  passes none of them, so chat is unchanged. Blocks over 60,000 characters
  render unhighlighted.
- Long blocks show their first lines (40 for code, 30 for results) with
  **Show all**. Nothing is truncated: **Download logs** still exports the full
  session JSON.
- `toolMeta` recognizes a few multi-word MCP server names (`ses_outbound`,
  `fast_io`, `s3_feeds`, …). Any other server splits at its first underscore,
  which matches the single-word bundle convention.
