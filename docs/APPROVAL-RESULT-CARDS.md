# Approval result cards: the completed card reads the call's own result

Design note for `agent_policy.critical_tool_result_cards`. For chosen critical
tools, the tool's result can carry a structured `result_card`. Once the
approved call returns, fleet checks the card strictly and stores it with the
approval. The settled card then reads the way the designer's completed card
does: a one-line outcome, a row per record with both of its ids, a status and
a link, plain failure wording, **Retry failed (N)**, and the raw JSON under
**Details**. The same applies to a notify-mode record. This note also records
the readable pending card's new layout (the designer's "Card design"). It is
recorded as
[ADR-0084](adr/0084-a-settled-card-reads-its-outcome-from-the-calls-own-result.md),
and it builds on [APPROVAL-CARD-DESCRIBERS.md](APPROVAL-CARD-DESCRIBERS.md)
(ADR-0082), [GROUPED-APPROVALS.md](GROUPED-APPROVALS.md),
[APPROVAL-PROGRESS.md](APPROVAL-PROGRESS.md) and
[RESUME-AFTER-APPROVAL.md](RESUME-AFTER-APPROVAL.md).

## Why

The first live chat booking (2026-10-11) approved one Index Exchange create.
The pending card read in plain words. After approval it became
`Applied · Create 1 deal on Index Exchange …` over forty lines of raw JSON,
and the deal's two ids and its link sat inside that JSON. The designer's
"Completed card" (Approval Flow and Card Requirements, pages 9 to 11) asks
for:

- a one-line summary;
- for creates, both ids per deal (buyer-facing and internal) with a link;
- the failure states in plain words: "Not applied: <reason>", "Applied, but
  the read-back didn't match", and, for an unclear create, "Checking whether
  this deal was created";
- the raw JSON collapsed under Details;
- for plans, "Retry failed (N)", which for creates covers only deals
  confirmed not created.

Only the bundle knows what its result means. The result already says it, so
fleet reads the card from there (ADR-0084 explains why fleet doesn't call a
second describer).

## What shipped

### The bundle opts in, per tool

```yaml
agent_policy:
  critical_tools: [execute_plan, next_campaign_id]
  critical_tool_modes: {next_campaign_id: notify}
  critical_tool_result_cards: [execute_plan, next_campaign_id]
```

- Suffix matching like `critical_tools`: the tool name equals the suffix or
  ends in `_<suffix>`, so a named-account variant is covered.
- A member must be a critical suffix. It must not be a handler-only card
  (`schedule_task`, `manage_tasks`, `preview_email`,
  `suggest_advanced_model`), because those return no tool result to read.
  Boot drops a bad member with a log line, and `fleet validate-config`
  reports it in the `agent_policy` floor check
  (`agentcore.ResultCardProblems`, the same code). Notify mode is allowed;
  see "Notify records" below.
- Installed at boot from `cmd/fleet/main.go`.
- **Absent key = today's behaviour, exactly.** No result is scanned, no row
  gets a card, and no payload gains a `result_card` key.

### The result card (the bundle contract)

The tool's result is a JSON object. Its top-level `result_card` key holds:

```json
{
  "outcome": "partial",
  "title": "1 of 3 Index Exchange deals created · 1 failed",
  "subtitle": "One deal was refused and one was left out.",
  "items": [
    {
      "key": "Acme_IX_Display_US",
      "label": "Acme_IX_Display_US",
      "id": "IX151710246087067804",
      "internal_id": "666624",
      "link": "https://app.example.com/deals/666624",
      "link_label": "Open in Index Exchange",
      "status": "done",
      "note": "Verified: Index Exchange reads it back as created.",
      "changes": [{"label": "Floor", "before": "$0.10", "after": "$5.00"}],
      "settings": [{"label": "Status", "value": "Active"}],
      "flags": [{"code": "deal_active", "label": "Deal is Active"}]
    },
    {"key": "Acme_IX_OLV_US", "label": "Acme_IX_OLV_US", "status": "failed",
     "note": "Index Exchange refused the floor. Nothing was created."}
  ],
  "footer": "Plan dp_01… r2",
  "retry": {"keys": ["Acme_IX_OLV_US"]}
}
```

- **`outcome`** (required) is one of:
  - `done`: every record applied, or already done;
  - `partial`: some applied, some not;
  - `failed`: nothing applied;
  - `mismatch`: applied, but the read-back didn't match;
  - `unclear`: the caller can't tell yet.

  It sets the card's tone: success, the accent, danger, or warning (for both
  `mismatch` and `unclear`).
- **`title`** (required) is the one-line outcome, at most 200 characters.
  `subtitle` and `footer` are optional, at most 2000 each.
- **`items`** (required, may be empty, at most 500) has one row per record:
  - `label` and `status` are required, and so is a non-blank `note` on a
    `needs_check` row: it is what the person should check.
  - `status` is one of `done`, `failed` (confirmed not applied), `mismatch`
    (applied, but the read-back differs), `needs_check` (applied, but
    something after it needs a person's check, such as a follow-up step of the
    same record that failed), `unclear` (it may or may not have happened),
    `skipped` (nothing to do, already done earlier) or `not_run` (never
    attempted: left out, or stopped before it).
  - Fleet words each status the same way on every bundle. The chip reads
    "Done", "Not applied", "Check didn't match", "Needs a check", "Outcome
    unclear", "Already done" or "Not run". The note's lead reads "Not
    applied:", "Applied, but the read-back didn't match:" or "Applied, but
    needs a check:". Use `needs_check`, not `mismatch`, for a record that was
    created when a later step of it failed (for example, the deal exists but
    its exclusion list did not apply). `mismatch` claims a read-back
    disagreed.
  - `key` is the record's key in the call: what a retry names. It is optional
    and unique within the card.
  - `id` is the buyer-facing id. `internal_id` is the system's own.
  - `link` is an https link. `link_label` names it ("Open in Index
    Exchange"), and is allowed only with a link.
  - `note` is the plain-words detail.
  - `changes`, `settings` and `flags` follow the describer card's rules.
- **`retry`** is optional: `{keys: [...]}`, with at least one key and at most
  500. Every key must name an item of the card, once, and that item's status
  must be `failed` or `not_run`.
- **Consistency:** a `done` card may not carry a `failed`, `mismatch`,
  `needs_check` or `unclear` row, and a `failed` card may not carry a `done`,
  `mismatch` or `needs_check` row. A card whose rows are applied but need a
  check takes `mismatch` (the warning tone) or `partial`; an `unclear` card
  may carry a `needs_check` row only beside an `unclear` one.
- **Older fleets** reject a card with a `needs_check` row (an unknown status),
  and the settled card then shows without it. A bundle should emit
  `needs_check` only against a fleet that has it.
- **Text rules and limits are the describer card's:** no nulls, no unknown
  keys, no control, bidirectional-formatting or line-separator characters,
  labels at most 200 characters and other text at most 2000, at most 100
  changes and 100 settings per item, at most 20 flags per item, at most 5000
  rows, and at most 128 KiB for the card. Fleet decodes a result of up to
  4 MiB to look for the card; a larger result keeps today's card.

`parseApprovalResultCard` (`internal/httpapi/approval_result_card.go`)
enforces all of this. `parseApprovalResultCardData` (`web/…/history.ts`)
checks it again on the client.

### When the approved call returns (`executeClaimedApproval`)

1. The approved call runs exactly as before. Its result passes through the
   post-execution governance once (`GovernAndBoundModelVisibleToolTextFull`),
   which now also returns the governed text before the size bound.
2. If the call ran without error, governance didn't block it, and the tool is
   opted in, `recordApprovalResultCard` looks for `result_card` in that
   governed text (its first JSON value, so trailing hook context does not hide
   it). It validates the card, refuses it if it carries the redactor's marker
   (`[REDACTED…]`: governance already scrubbed a secret out of it) or if the
   secret redaction would still change it, and writes the canonical JSON with
   `store.SetApprovalResultCard` (migration 077,
   `approvals.result_card_json`, nullable with no backfill). The write lands
   once, and only on the owner's row while it is still executing.
3. `SetApprovalResult` then records the outcome and the history breadcrumb, as
   before. The approve POST answer gains `result_card` when one was stored.

Any failure leaves the card off and is logged (passed through the secret
redaction first). The outcome is recorded either way.

### Where the card travels

- `result_card` (the canonical object) is added to the approve POST answer and
  its idempotent replays, to each `resolved_approvals` row of the
  conversation GET, to the one-card GET (which the progress poll reads), and to
  the group decision's per-card `result`.
- It is served only on a success fleet recorded: approved with `is_err` false,
  which covers a finished call or a notify record. It is re-validated on every
  read, so a failed call, an outcome not recorded, a running call, or a row
  edited by hand never shows one.
- The model never gets a second copy. Its tool result is the tool's own text,
  which already contains the card.

### Notify records

A notify-mode record (#1153) is written before its call runs, so it has no
result yet. For a member of `critical_tool_result_cards`:

- `checkCriticalToolApproval` remembers the recorded call (tool, input, call
  id).
- When the loop records the call's result (`InteractivePolicy.RecordToolResult`,
  on the same governed, unbounded text), it hands that result to the stager
  once (`agentcore.ActionResultRecorder.RecordActionResult`).
- The stager stores the card on the record row (an approved row, `is_err`
  false, with the record text) and pushes it to the open card as
  `tool.action_record_card {approval_id, tool, result_card}`. That event is
  under the `approval_cards` SSE capability, so a client that opted out of
  approval cards does not get it.
- A failed call, an unknown call id, or a transport without the interface
  changes nothing.
- A record never offers **Retry failed**. A notify tool runs without an
  approval, so a retry of it would run straight away rather than wait for a
  new approval. The stager leaves the card's `retry` out, the read path
  refuses to serve a record card that has one, and the web hides the button on
  a record (Codex P1 on #1722).
- The result hook carries no call id, so a result is matched by tool and
  input. If two recorded calls share both (parallel identical calls can finish
  in either order), neither gets a card, so a card never lands on the wrong
  record.

The record then reads "Campaign ID ELC07375 allocated · ran without asking".
It keeps its record line and has its raw arguments (an idempotency hash, for
example) under **Details**.

### The completed card (web: `ApprovalResultCard.tsx`)

`GenericActionCard` renders `ApprovalResultCardView` for a settled success
that carries a result card:

- **The header** is the card's title, with an icon in the outcome's tone (none
  for a `partial` plan). Under it, **Approved: <the pending card's title>**
  keeps what the person approved in view.
- **The records:**
  - One record reads in an inner box: its name, a status chip, the
    monospace buyer-facing id, "internal <id>", the link (by its
    `link_label`), and its note. Its changes and settings are listed below the
    box.
  - Several records read as rows, each with a status icon (a dashed ring for
    `not_run`).
  - Above 10 records, the done ones fold into one "✓ N records done ·
    **View all**" row, so the failures stay in view.
- **Status words and failure lead-ins are fleet's**, the same on every
  bundle: **Done**, **Not applied**, **Check didn't match**, **Outcome
  unclear**, **Already done** and **Not run**, with notes that lead "Not
  applied: …" and "Applied, but the read-back didn't match: …". The bundle
  writes the titles and notes ("Checking whether this deal was created").
- **Retry failed (N)** appears when the card names retry keys. It sends
  `retryFailedPrompt` as a user turn: "Retry the N failed items from the
  approved <tool> call (approval_id=…): "k1", …. Retry only these, and stage
  the retry for my approval." It is sent as a card's own request, so it
  neither takes nor clears the composer's draft or attachments. It then
  disables itself, unless the turn was not accepted (a model error, an
  upload or network failure), in which case it offers itself again. Nothing runs from the
  click: the agent stages a new call, which needs a new approval, the same
  path as **Ask again** on a timed-out card.
- **Details** (collapsed, on the action row) holds the pending card's records,
  the `via <server> · <tool>` line with the arguments, and the raw result.
- A notify record keeps the muted record chrome and shows its record line.

A settled card without a result card (a decline, a failure, an outcome not
recorded, a tool that didn't opt in, or a card that failed a check) renders
exactly as before.

### The pending card's layout (web, readable cards only)

A pending card with a describer's readable card (ADR-0082), not yet running,
now follows the designer's "Card design" (`ApprovalReadableCard.tsx`):

- An **APPROVAL NEEDED** eyebrow, with the countdown top-right ("Auto-declines
  in 58 min", then m:ss in the last two minutes, and the timed-out notice).
- The title and subtitle.
- One record in an inner box: its name, the monospace buyer-facing id, and
  **Open in <system> ↗**. The link uses the new optional `link_label` on a
  describer item, else **Open**.
- Its changes and settings as label … value rows, with before → after
  right-aligned. After 8 rows the rest collapse behind **Show all N**.
- Several records read as rows with their one change on the right. They keep
  **Show N more** after 10 and search above 25.
- **Approve** and **Cancel**, with **Details** on the same row. Details opens
  the raw tool and arguments below the row.
- The seat badge, the submit error, **Ask again** and apply-all are unchanged
  (apply-all is still withheld for a `critical_tool_no_session_approval`
  tool).

It checks out in both themes and at 380 px (screenshots in the PR). Generic
cards, email, shell and task cards are unchanged. A running card, and the
rows of a grouped card, keep their layout.

`link_label` is the one addition to the describer schema (ADR-0082). It is
optional text of at most 200 characters, and allowed only with a `link`. An
older fleet refuses a card that carries it and shows the generic card
instead, so a bundle should send it only after this release.

## Deviations

- **No result describer.** The request offered either a second describer call
  with the staged arguments and the result, or a schema-checked payload in the
  result. ADR-0084 records why the payload was chosen. It needs no call, no
  scope, and no gates outside the loop, and it can't disagree with the result
  it describes.
- **The approved settings stay under Details, not in the card's face.** The
  completed card shows the pending card's title. For a create, its settings
  (buyer, floor, fee and so on) move under **Details**, as the mockups' compact
  completed card does. The records the result names, with their new ids, are
  in the face.
- **Retry is a canned user turn,** like **Ask again**. It doesn't edit the plan
  or pre-select keys anywhere: the agent maps the keys to its own retry call.
- **The pending layout applies to every readable card,** not only to opted-in
  result cards. A describer card is itself the bundle's opt-in (ADR-0082), and
  the request scoped the chrome to "only when a describer card is present".
  The **Approve & run** button reads **Approve** on these cards.

## Deferred (noted, not built)

- **Stop mid-run** shipped later: see [APPROVAL-STOP.md](APPROVAL-STOP.md).
  Still deferred: per-deal **One at a time**, and progress counts on the
  card ("12 of 24 done, 1 failed" as a split bar). The progress line from
  APPROVAL-PROGRESS.md still shows while a call runs.
- **Download results** (mockups 6 and 6b).
- **Grouped completed card.** After **Approve all**, each call still settles
  on its own card (GROUPED-APPROVALS.md). Each one can now be a result card.
- **The terminal client ignores `result_card`** and prints the result text as
  before.
- **Hot reload:** like every `agent_policy` value, the list is read at boot.
- **Adoption:** bundles adopt the key after this release, because the manifest
  decoder is strict and an older fleet refuses a bundle that carries it. The
  `result_card` field inside a result is harmless to an older fleet, which only
  shows it in the raw result.
