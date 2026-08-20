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
| Email send | `send_email`, `*_send_email` | Send / Cancel (+ apply-all) |
| Email preview | `preview_email` | Dismiss only — display-only by design |
| Bash | `bash` (risky commands) | Approve & run / Cancel |
| Schedule | `schedule_task` | Approve & schedule / Edit… / Cancel |
| Manage tasks | `manage_tasks` | Approve & stop/update / Cancel |
| Advanced-model nudge | `suggest_advanced_model` | Switch & retry / Just switch / Dismiss |
| **Generic action** | everything else (bundle critical tools, e.g. a pages deploy) | Approve & run / Cancel (+ apply-all) |

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

A **notify-mode record** (#1153) renders on the same chrome in an
informational form: muted border, title "… · ran without asking", no buttons,
and the persisted record text — including the bundle-authored undo hint — as
its body. It never masquerades as a past human approval.

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
