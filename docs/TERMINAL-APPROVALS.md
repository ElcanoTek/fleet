# Terminal chat and scheduled-task approvals

`fleet chat` is a thin client of the governed chat API. New conversations use
the workspace's advertised default model unless `--model` is supplied. Resumed
conversations retain their stored model. One-shot output reports the conversation
ID on stderr so automation can resume the same thread.

## Review and resolve

- `/approvals` shows pending cards, full server-provided summaries, expiry
  deadlines, and local edits. `/approvals reload` refreshes server state.
- `/edit {"name":"Report","prompt":"...","cron":"0 9 * * *"}` edits the oldest
  scheduled-task card. Omitted fields stay unchanged. `cron` may only change a
  recurring card; adding it to a one-time/`run_at` card, or clearing it on a
  recurring card, is rejected locally so approval cannot claim the card and
  then fail validation. Edits are local until approval; the server revalidates
  them before creating a task.
- `/approve` or `/deny` resolves the oldest card. Supply its full ID to select
  another card explicitly.
- `/approve [id] session` or `/deny [id] session` applies that decision to future
  calls to that tool in this conversation. `pattern arg=glob` narrows it to
  matching arguments. Executable-tool policies live in server memory and reset
  on restart. Handler-only tools (including schedule_task and manage_tasks) use
  a terminal-session policy: each matching card still resolves through the real
  approval endpoint. These decisions end when the terminal exits and are keyed
  by conversation, so switching threads cannot authorize another thread's work.
  Policies accumulate and a matching deny takes precedence. Patterns match
  string arguments by their original names (for example, `name`, `cron`, or
  `to_email`, not the email summary's `to` label). The terminal shows the
  handler-only argument strings supplied by the server; older servers without
  that metadata cannot support handler-only patterns. Spaces are allowed in a
  pattern, such as `/approve pattern cron=0 9 * * *`.
- `/resume <conversation-id>` switches to an owned conversation and reloads its
  cards. Starting with `--conversation` also loads cards. It does not load the
  historical transcript into the terminal; server-side model context is retained.
- One-shot `--conversation <id> --approve <approval-id>` and `--deny` settle one
  card. These alternatives are mutually exclusive. Expired/rejected approvals
  and tool-execution failures exit nonzero.

Approval requests are serialized with sends and conversation switches. Transport
failures retain the card for retry; server idempotency prevents replaying resolved
actions. Expired cards stop appearing pending. Approval requests have an
eleven-minute transport deadline, exceeding the ten-minute staged bash window.
All decisions use the existing server authorization and approval gates.

### Execution outcome fields

`status` on an approval is consent (`pending` / `approved` / `rejected`), not
whether the staged tool succeeded. The POST body and GET `resolved_approvals`
echo the same extra keys so a retry or reload cannot look like success:

| Field | When | Client |
| --- | --- | --- |
| `is_err` | Execution finished (`true` = failed, `false` = succeeded) | Failure is an error; `is_err: false` is the only success |
| `executing` | Claim won, tool still running (`result_text` is the named sentinel `Approved — executing…`) | Not success. TUI returns `approvalRunningError`, keeps the card, empty status. `/approve <id>` is idempotent result retrieval. Web hydrates `executing` on the pending card: no deny/edit/apply-all, expiry countdown suppressed, **Check result** retrieves the outcome. |
| `execution_unknown` | Approved row with no stored `is_err` (legacy) | Not success. TUI errors with the authoritative status; web shows “outcome not recorded”, never “Email sent ✓”. |

Do not infer failure by parsing `result_text`. In-flight rows also appear on
GET `resolved_approvals` with `executing: true` so `/approvals reload` can
re-attach them; they are exempt from local expiry and auto-decision. Handler-only
tools (schedule_task, manage_tasks, preview_email, suggest_advanced_model) never
receive the executable-tool auto-approve sentinel.

## Scope and verification

This closes the initial terminal deferrals: scheduled-task editing,
session/pattern decisions, and switching conversations to resolve saved cards.
Edit fields match the existing web endpoint (name, prompt, cron). Terminal review
automatically displays the full frozen email summary, including all recipients,
CC/BCC, attachments and content, in escaped JSON before offering a decision.
If the server flags `content_overflow` (body over the 1 MiB summary cap),
approval is refused so a hidden tail cannot be sent. Bash cards likewise print
the complete frozen command, control-escaped, rather than a 120-rune prefix.
One-line summaries sanitize terminal control bytes.
One-shot settlement fetches the current card and prints its email review before
the approval request. `/approvals` also exposes full summaries for every tool.

Tests cover expired/failed outcomes, retry after transport failure, rehydration,
editing, scoped requests, superseded cards, conflicting flags and prefixed email
names. Live dev checks exercised scheduled calculation and two-MCP analysis jobs.

Those live tests also exposed concatenated pre-audit/final answer drafts. The
runtime now prefers the final completed response when available, preserving prior
work in the transcript carried into enforcement rounds. When the run finishes, the live stream emits `text.replace` with that
authoritative text so web, TUI, and one-shot clients drop superseded
pre-audit drafts; a reload and a live view then agree. One-shot stdout is
the reconstructed final text, not every intermediate delta. Abort and
round-cap paths keep the partial transcript. The production email summarizer includes frozen
`attachments` and `inline_attachments` metadata (paths and CIDs, not file
bytes) so terminal review cannot hide a workspace file that `/approve` would
send.
