# Approval cards: the human-review surface in chat

Approval cards are the inline cards a chat turn stages when the agent wants to
do something a human must see first (send an email, run risky bash, create or
change scheduled tasks, any bundle-declared critical tool) or has just done
under notify mode (#1153). This page records the 2026-08 rework of how those
cards behave over their whole lifetime — staging, waiting, timing out,
resolving, and re-appearing after a reload — and what was deliberately left
alone. The timeout mechanics live in
[AGENT-RUNTIME.md](AGENT-RUNTIME.md) ("Approval timeouts" and "Per-tool approval
MODE"); this page is the card UX.

## The card classes

| Card | Tools | Pending actions |
| --- | --- | --- |
| Email send | `send_email`, `*_send_email` | Send / Cancel (+ apply-all, unless the bundle requires a decision per call) |
| Email preview | `preview_email` | Dismiss only — display-only by design |
| Bash | `bash` (risky commands) | Approve & run / Cancel |
| Schedule | `schedule_task` | Approve & schedule / Edit… / Cancel |
| Manage tasks | `manage_tasks` | Approve & stop/update / Cancel |
| Advanced-model nudge | `suggest_advanced_model` | Switch & retry / Just switch / Dismiss |
| **Generic action** | everything else (bundle critical tools, e.g. a pages deploy) | Approve & run / Cancel (+ apply-all, unless the bundle requires a decision per call) |

### The generic action card (new)

Any critical tool without a tailored card used to **fall through to the email
card**: a pages deploy staged as "ACTION REQUIRED · Send this email?" with a
"(no subject)" header, resolved as "Email sent ✓", and was declined into
history as "User declined to send this email". That is exactly the wrong copy
on the one card class whose entire job is telling a human what is about to
happen. The generic card renders instead:

- the humanized action and its server ("Run \"Deploy page\"?", `via pages ·
  mcp_pages_deploy_page`),
- the call's **top-level arguments verbatim** (sorted keys, values compacted
  and rune-truncated server-side; unparseable args fall back to the raw
  payload) — fleet cannot know what a bundle tool's arguments mean, so showing
  them unedited is the honest floor for a review step,
- action verbs end to end: `Approve & run` / `Cancel`, resolved as
  `completed ✓` / `cancelled` / `timed out` / `failed`, and rejection history
  that names the tool instead of claiming an email was involved,
- the seat badge as "Runs as … on …" (the email card keeps "Sending as"),
- the same countdown, apply-all (#300) and seat semantics as the email card.

A bundle can also give a critical tool a **readable card**: it declares a
read-only describer tool, fleet calls it when the call is staged, and the card
shows a plain-words title, the records with their ids and links, each change
as before → after, and flags, with the raw arguments under "Details". Any
describer failure leaves this generic card. See
[APPROVAL-CARD-DESCRIBERS.md](APPROVAL-CARD-DESCRIBERS.md).

A **notify-mode record** (#1153) renders on the same chrome in an
informational form: muted border, title "… · ran without asking", no buttons,
and the persisted record text — including the bundle-authored undo hint — as
its body. It never masquerades as a past human approval.

## Tools that need a decision per call (no apply-all)

### Why

The email and generic cards carry a small checkbox below the buttons, "Apply
my choice to all <tool> calls in this chat" (#300). Ticked, the click
registers an in-memory session policy, and every later call of that tool in
the conversation skips its card until the process restarts. That is right
for a run of similar, reversible actions, and wrong for an action where each
call is a separate commitment (a record create or update in an external
system, for example): one tick turns every later call into an unreviewed
one. A bundle needs a way to say "this tool always gets its own card".

### What shipped

```yaml
agent_policy:
  critical_tools: [create_deal, update_deal]
  critical_tool_no_session_approval: [create_deal, update_deal]
```

- `agent_policy.critical_tool_no_session_approval` is a list of bare
  suffixes, matched exactly like `critical_tools` (the tool name equals the
  suffix or ends in `_<suffix>`, so a named-account variant is covered by its
  base suffix). It rides `Bundle.AgentPolicy()` into
  `agentcore.ConfigureAgentPolicy` at boot (`cmd/fleet/main.go`; the
  scheduled `fleet run` path stages no cards and does not install it), and
  `agentcore.SessionApprovalAllowed(tool)` is the one predicate every layer
  asks.
- **The card hides the checkbox.** `approvalClientFields` adds
  `no_session_approval` (a boolean) to the live `tool.approval_required`
  event and to every `pending_approvals` row. The web email and generic cards
  render no apply-all checkbox when it is true, and always post
  `scope: "once"`. Approve, Cancel, the countdown and the seat badge are
  unchanged.
- **The server refuses a wider scope.** `handleApproval` answers a pending
  card's POST with any scope other than `""`/`"once"` (`session`, `pattern`,
  `pattern:<arg>=<glob>`, approve or deny) with 400 and "this action needs its
  own decision for each call …; use scope once", before anything is claimed,
  so the card stays pending. A settled card still answers its recorded
  outcome, as before. `maybeRegisterSessionPolicy` also skips such a tool, in
  case a later caller reaches it without that check. The terminal client
  shows the 400 text and keeps the card.
- **Staging ignores a session policy for it.** `approvalStager.Stage` skips
  the registry for a matching tool, so a policy registered before the rule
  existed (or by any path that missed the refusal) cannot pre-approve or
  pre-deny a call: each one stages its own card.
- **`fleet validate-config` checks the list.** The `agent_policy` floor check
  reports a member that is not a critical suffix (the same merged list the
  audit gate uses, base email suffixes included), because no card is ever
  staged for such a tool and the entry would do nothing. At boot the same
  problem is one log line, and the member is still installed: refusing a
  session scope is the safe direction.

### Deviations and deferred

- **Deny-all is refused too.** A session pre-denial is harmless on its own,
  but the rule is "one decision per call" and the checkbox serves both
  buttons, so the server refuses every non-`once` scope rather than splitting
  the two.
- **`FLEET_AUTO_APPROVE_IN_TEST` is untouched.** It is a test-only escape
  hatch, not a session policy, and still auto-approves executable tools.
- **The terminal client does not pre-check the flag.** It sends what the
  user typed, and the server's 400 is the answer. Hiding `/approve <id>
  session` locally would need the flag threaded through two TUI event
  parsers for no change in what is enforced.
- **Boot-time only**, like every other `agent_policy` value: a change needs a
  restart. Bundles adopt the key after the release that understands it,
  because the manifest decoder is strict.

## The schedule card names the task's connectors

A task scheduled from chat — by the agent's `schedule_task` call or by
promote-to-task — inherits the conversation's connector selection
([ADR-0068](adr/0068-chat-scheduled-tasks-inherit-connectors.md)). The stager
snapshots the opted-in optional servers (with their seats) and the user's
hosted connections into the staged args as `connectors`, plus the bundle's
available always-on servers as `always_on_connectors`; the summary exposes both
and a `no_connectors` flag. The card renders a `Connectors:` line, or
`always-on only (…)` when nothing was selected but the bundle binds servers
anyway, and a warning box when the task would run with no connector at all —
the failure that used to surface only as a self-audit abort on the first run.
The chat confirmation after approval repeats the connector list. Cards staged
before the snapshot existed carry no keys and render the warning.

## Resolved cards survive reload

The conversation GET used to return only *pending* approvals, so every
resolved card ("Email sent ✓" with its humanized delivery details, a schedule
confirmation, a notify record) existed only for the live SSE stream and
vanished on the next reload — the transcript silently changed shape the first
time the user left and came back. For notify mode this broke the feature's own
contract: its entire audience is the user who walked away, and the "ran
without asking" card plus undo hint reached only the stream nobody was
watching.

The GET now also returns `resolved_approvals` (capped at the most recent 100
per conversation — approvals are one row per human-gated action, so the cap is
a pathological-payload bound, not a UX limit). Each card re-attaches to the
message holding its `tool_call_id`, falling back to the last assistant message
for rows that predate call-id capture, mock turns, and promote cards. Notify
records are recognized by their stable `"Ran without asking"` result prefix
(the approvals table stores no mode column; the prefix is the durable trace of
how the row resolved) and carry `recorded: true` so the card renders its
informational form.

## Timeouts: what changed and what did not

Default-deny on timeout stays (#225) — it is the right posture for real
actions. What changed:

- **The default window is 3600s, not 300s**, and the global layer is now
  admin-settable live (`approval_timeout_seconds`, Settings → Admin →
  Features, 60s–24h). Five minutes measured from *whenever the agent staged
  the card* — often deep into a run the user had reasonably stopped watching —
  mostly denied the final, wanted action (the same observation that motivated
  notify mode). Per-tool bundle windows and the per-conversation override
  still win over the global layer.
- **`preview_email` never expires.** It stages with `expires_at = 0`: the card
  is display-only, so there was never an action for default-deny to protect
  against. Before this, the sweep "auto-denied" previews and wrote "the action
  was not taken" into history — misleading the model (the preview WAS
  displayed) and the user, whose rendered draft flipped to a timeout notice
  while they might literally be reading it. Legacy pending preview rows that
  still carry a deadline sweep with honest copy ("Preview closed. It was
  display-only; nothing was sent.").
- **A click after the deadline resolves the card as timed out, immediately.**
  It used to lose the atomic claim, get the row's still-`pending` state echoed
  back, and silently reset — clickable forever until the next sweep tick. The
  handler now claims the expired row with the same primitive the sweep uses
  (default-deny stays authoritative; nothing executes) and the card settles.
  Cancel/Edit buttons also disable at expiry, matching Send.
- **Timed-out cards offer "Ask again."** One click submits a user turn asking
  the agent to re-stage the action (the old card's claim is spent and its
  arguments may be stale, so re-staging — not re-arming — is the only honest
  recovery). Recovery used to require composing that request by hand.

## How long an approved action may run

An approved MCP call runs under a budget: 60 s by default, or what its server
declares in the bundle (`approved_call_timeout_seconds`, scaled for a
`deal_ids` batch). A call on a server that declared one and is still running
after about 50 s answers the approve POST with `executing`, so the card shows
its still-running state and **Check result** fetches the outcome once it is
recorded. See [APPROVED-CALL-BUDGET.md](APPROVED-CALL-BUDGET.md).

## The agent can carry on once a card is settled

For tools a bundle lists in `agent_policy.critical_tool_resume`, a settled
card (approved and its result recorded, declined, or timed out) starts one new
turn in the conversation once no other card there is pending or executing, so
the agent verifies the outcome and continues without the user typing. The turn
runs through the input queue (a running turn makes it wait), its input renders
as a "Continued automatically after an approval" notice, an hourly cap bounds
it, and a restart drops a due resume with a note instead of running it. The
approve/decline answer says `"resume": true` for such a card so the web
follows the conversation for that turn. See
[RESUME-AFTER-APPROVAL.md](RESUME-AFTER-APPROVAL.md) and ADR-0083.

## One card for the actions one turn stages

For tools a bundle lists in `agent_policy.critical_tool_group_approval`, the
cards one turn stages carry the turn's `group_id`, and two or more pending ones
render as one card: **N actions to approve**, a checkbox per call (its readable
card or arguments, its seat badge), one countdown on the earliest deadline, and
**Approve all (k)** (the checked calls approved, the unchecked declined, in one
request to `POST /conversations/{id}/approval-groups/{groupId}`), **One at a
time** (the individual cards) and **Cancel all**. Each call is still decided by
the same code as its own card, with its own claim and execution. See
[GROUPED-APPROVALS.md](GROUPED-APPROVALS.md).

## Honest scope / deliberately not done

- **No approvals-table mode column.** Notify records are tagged by their
  result-text prefix, shared as one constant between the writer and the reload
  marshaller. A column is the cleaner design if a second consumer ever needs
  the distinction; one display consumer did not justify a migration.
- **Resolved statuses are the row's statuses.** A send that was approved and
  then failed in the provider stays `approved` with the failure in
  `result_text` (the DB never stored a `failed` status); the card renders the
  failure text honestly.
- **Live SSE for the sweep is still not emitted.** A card that times out while
  the transcript is open greys out via the client countdown; its rejected
  state lands on the next reload. The expired-click fix makes any interaction
  settle it immediately, which removes the confusing half of that gap.
- **Card placement on reload anchors to the staged `tool_call_id`.** Rows
  without one (pre-capture rows, promote cards, mock turns) keep the previous
  last-assistant-message fallback.
