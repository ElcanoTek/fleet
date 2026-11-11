# Editing a scheduled task's prompt from chat

`manage_tasks` (#1152) changes or stops tasks that already exist. Until this
change its only prompt change was a full replacement, and the model had no way
to read the prompt it was replacing. In production a user asked the assistant
to add a trend section to a scheduled email. The model rebuilt the prompt from
what it remembered of the conversation and sent it as a full replacement.
The rules the working prompt had picked up over time (an API-field gotcha, a
filter that must never be passed, columns, fallbacks) were silently dropped.
The approval card showed only `prompt → replaced (<first 80 characters>)`, so
the user could not see what would be lost before clicking Approve.

## What shipped

- **`action="show"` (read-only, no card).** Returns the current prompt,
  schedule, timezone, model, tags and status of the tasks a selector names
  (`task_ids` or `match`, same rules as `update`). It runs through the same
  seam an approved update runs through (`httpapi.TaskMutationRequest` →
  `taskManagerProvider`), so the selector resolution, the 25-task cap and the
  ownership rule are the update's own. A `match.model` selector, applied in
  memory, now pages through every task the storage filter returns instead of
  only the first page, so a match past the first page is not silently dropped
  (this fixes `update` too). The model sees the prompts of exactly
  the tasks the same selector could change. A task the person did not create
  is listed under `not_shown` by id with the reason and nothing of its content (no prompt, no title; this also applies to the `update` and `stop` reports). Each
  prompt is bounded at 20,000 characters (runes); a longer one is cut and
  flagged `prompt_truncated`. The interactive gate lets only `show` through
  (`tools.IsManageTasksShow`). The tool's `Run` serves that read and nothing
  else: every other action there is still the bypass tripwire, so letting
  `show` through cannot let a write through.
- **`prompt_edits: [{old, new}]` and `prompt_append`.** Both are applied to
  each task's **stored** prompt when the user approves, never to text the model
  retyped (`tools.ApplyPromptEdits`). Edits run in order. Each `old` must occur
  exactly once in the text the earlier edits left. If it does not, the whole
  task is skipped and left unchanged, and the report gives the reason ("prompt
  edit 2: the text to replace (…) is not in this task's prompt", or "appears
  more than once", overlapping matches included). It never replaces the first
  match, and it never leaves a task half edited. The edited prompt must still
  fit the API's prompt bounds (3 bytes to 250,000 bytes), so an edit that
  empties a prompt is refused for that task. `prompt_append` adds a paragraph after a blank line. Both are refused
  next to a full `prompt`, and an update carries at most 20 edits. With a
  `match` selector the edits apply to every matched task separately. Tasks
  whose prompt matches are changed; the rest are skipped by name.
- **The card shows the text.** The summary carries `prompt_edits` (each edit
  as the text it replaces and the text it becomes), `prompt_append`, and
  `prompt_replacement` (the whole new prompt of a full replacement, under a
  heading that says it replaces everything). Each block is bounded at 6,000
  characters by runes, never bytes. A cut block ends with "… [shortened on this
  card]". The change line names the kind: "REPLACED IN FULL (N characters):
  anything not in the new text is removed" versus "N exact-text edits; the
  rest of the prompt is kept".
- **The tool description** tells the model to `show` a task before changing
  its prompt, to use `prompt_edits` / `prompt_append` for an adjustment, and to
  send a full `prompt` only when the user asked for a rewrite.

The approval gate for every write is unchanged: `update` and `stop` still
stage a card, and nothing is written until the user clicks Approve.

## Deliberately not done

- **No resolved before → after for a full replacement.** The card still
  renders the selector, not a resolved task list (see `summarizeManageTasksInput`),
  so it cannot show each task's current prompt next to a full replacement. It
  shows the whole new text and says plainly that everything else is removed.
  The steer toward `show` + edits is what keeps a "slight adjustment" from
  being a rewrite. A full replacement is still allowed, because sometimes a
  rewrite is what the user asked for.
- **Prompt-edit results are not pre-validated before the card.** A full
  `prompt` is still checked for a malformed `EXECUTION REQUIREMENTS` line
  before staging (#1601). An edited prompt only exists once the edits are
  applied to the stored text, so it is checked at approval by
  `UpdateEditableTask`, and a failing task is skipped with that message.
- **No compare-and-swap on the stored prompt.** As with every other
  `manage_tasks` field, the edit is computed from the task as read when the
  approved call runs and written through the row-locked
  `UpdateEditableTask`. A prompt changed by someone else between that read and
  the write (the same request) is overwritten, which is the window the existing
  fields already had.
- **Terminal client.** `fleet chat` renders the card from the same summary.
  Its one-line card text is unchanged and does not show the edit blocks.
