# Approval-card describers: readable cards for critical calls

Design note for the readable approval card. A bundle can name a read-only
"describer" tool for a critical tool. When that call is staged for approval,
fleet calls the describer and renders its structured answer as a card a
person can read: what the action is, which records it touches, each change as
before → after, and the flags a reviewer must not miss. It extends
[APPROVAL-CARDS.md](APPROVAL-CARDS.md) (the generic action card this
replaces when a describer answers) and is recorded as
[ADR-0082](adr/0082-approval-card-describers-run-a-declared-read-at-staging.md).

## Why

A bundle critical tool without a tailored card renders on the generic action
card: the humanized tool name and its top-level arguments verbatim. That is the
honest floor, because fleet cannot know what a bundle tool's arguments mean.
For a record write it reads like `deal_id: 5f2c…`, `patch: {"floor":2.5}` and
`etag: W/"77"`. A trader approving the change has to reconstruct which deal it
is, what the floor was, and whether the deal is live. The designer's
requirement R9 ("Approval Flow and Card: Requirements") asks for a card in
plain words: a title, the record (name, buyer-facing id, link), the change as
before → after, side effects and flags (e.g. "Deal is Active"), and the raw
tool, arguments and ETag collapsed under "Details". Approve, Cancel and the
countdown stay unchanged, and a completed card collapses to a one-line outcome.

Only the bundle's server knows what its call means and can read the current
values, so fleet asks it, through a tool the bundle declares.

## What shipped

### The bundle declares a describer

```yaml
agent_policy:
  critical_tools: [update_deal]
  parallel_safe_tools: [mcp_deals_describe_deal_update]
  critical_tool_card_describers:
    update_deal: describe_deal_update
```

- `agent_policy.critical_tool_card_describers` maps a critical suffix to a
  describer suffix on the **same server**. Keys match like `critical_tools`
  (longest matching suffix wins, so a named-account variant is covered).
- The rules (`agentcore.buildCardDescribers`):
  - The key must be a critical suffix.
  - The describer must **not** be critical under the merged suffix list (base
    email suffixes included). It runs without an approval, so a critical
    describer would be a write nobody approved.
    Mind the suffix rule: a describer named `describe_update_deal` **is**
    critical when `update_deal` is (it ends in `_update_deal`), and the loop
    would gate it as such, so it is refused. Name it so it does not end in a
    critical suffix, e.g. `describe_deal_update`.
  - The describer must match an entry in `parallel_safe_tools`, which is how a
    bundle declares a tool a read.
  - At boot an entry that breaks a rule is dropped with a log line.
    `fleet validate-config` reports the same problems in its `agent_policy`
    floor check (`agentcore.CardDescriberProblems`, the same code).
- Installed at boot from `cmd/fleet/main.go`. The scheduled `fleet run` path
  stages no cards and does not install it.

### Staging calls it (`approvalStager.describeApproval`)

When `Stage` is about to create the row for a matching MCP call:

1. It resolves the staged tool's registered server from the turn's catalog,
   then the describer on that server: the tool named exactly the suffix, else
   the single tool ending in `_<suffix>`. If none or several match, the call
   resolves to nothing. Fleet does not guess which read to run.
2. It re-checks the describer at call time with the loop's own predicates. It
   must not be critical (`IsCriticalTool` on the full registered name), and it
   must be parallel-safe under that name or, for a named-account seat, under
   its base server's name.
3. It calls the describer through the **turn's own MCP scope** (the broker,
   catalog and seat `BindTurnMCPScope` installed), with the staged arguments
   decoded with `json.Number`, so large integers pass through unchanged. The
   call is bounded by `approvalCardDescriberTimeout` (5 s), as both a context
   deadline and the per-call budget the broker child applies
   (`mcp.WithCallTimeout`). There are no retries. The child-side
   authorization (ADR-0042) applies as for any call: a describer missing from
   the server's tool allowlist is refused there and falls back.
4. It validates the answer strictly (below), checks that the secret
   redaction would not change it (`tools.RedactionWouldAlter`, the same check
   `show_ui` refuses a card on), and stores the canonical JSON on the new row
   (`SetApprovalCard`, pending rows only).

Any failure leaves the card off. That covers no describer on the server, a
timeout, a transport or tool error, output that is not exactly the schema, a
secret-like value, or a failed store write. The row, the event and the
countdown are then the generic card's, exactly as before. Describing never
blocks or fails staging. Each failure is logged with its detail and emitted
on the turn stream as `tool.approval_card_fallback`
`{approval_id, tool, reason}`, with `reason` one of `describer_unavailable`,
`timeout`, `describer_error`, `invalid_card`, `redacted` or `store_error`.

The describer's output never reaches the model: it is display data on the
approval row. The model's tool result for the staged call is the same
`APPROVAL_REQUIRED` text as before.

### The card schema

```json
{
  "title": "Update 2 deals",
  "subtitle": "Raise the floor on the Q4 package",
  "items": [
    {
      "label": "Q4 Video",
      "id": "PM-123",
      "link": "https://ssp.example.com/deals/123",
      "changes": [{"label": "Floor", "before": "$2.00", "after": "$2.50"}],
      "settings": [{"label": "Currency", "value": "USD"}],
      "flags": [{"code": "deal_active", "label": "Deal is Active"}]
    }
  ],
  "footer": "Changes apply immediately."
}
```

`parseApprovalCard` (`internal/httpapi/approval_card.go`) decodes it strictly:

- **Strict decoding.** It rejects unknown keys at any level, a value of the
  wrong type (a number where a string belongs), trailing data, and invalid
  UTF-8.
- **Required fields.** `title`, `items` (it may be empty), and each item's
  `label` are required. Each change needs `label` and `after`. `before` is
  optional: absent means the field had no previous value.
- **Strings are text.** Labels and titles are at most 200 characters, other
  text at most 2000. No string may contain a control character, a
  bidirectional-formatting character or a line/paragraph separator.
- **Links.** A `link` must be an absolute `https` URL with a host and no
  credentials.
- **Flags.** A flag `code` is a short lowercase token (`[a-z0-9][a-z0-9_.-]{0,63}`).
- **Size limits.**
  - 128 KiB of raw output, and the same for the stored card (up to 100 resolved cards ride one conversation GET).
  - 500 items.
  - 100 changes and 100 settings per item, 20 flags per item.
  - 5000 rows across the card.

The stored card is re-validated (and re-checked for redaction) on every read,
so a row from another version, or one edited by hand, never puts an
off-schema card on the wire.

### Where the card travels

- **Storage:** migration `071_approval_card.sql` adds a nullable
  `approvals.card_json TEXT`. NULL is "no card", the state of every existing
  row, so no backfill is needed. Every approval read returns it as
  `Approval.CardJSON`.
- **Wire:** `card` (the canonical object) is added to the live
  `tool.approval_required` event, to each `pending_approvals` row, to each
  `resolved_approvals` row, and to the one-card approval GET. A row without a
  card carries no `card` key, so every existing consumer reads the same bytes
  as before for it.
- **The chat-stream contract** recordings carry no approval turns yet (see
  [TESTING-STRATEGY.md](TESTING-STRATEGY.md), "Not covered yet"), so the frame
  is pinned by the Go stager test and the hand-written mocked Playwright
  payload, as the rest of the approval frames are.

### The web card (`GenericActionCard` + `ApprovalReadableCard.tsx`)

- **Pending:**
  - The header is the describer's title, followed by the subtitle.
  - Each record shows its label, its id (monospace) and an **Open ↗** link
    (new tab, `noopener noreferrer`).
  - Changes render as `label: before → after`, settings as `label: value`,
    flags as warning badges.
  - The footer follows the records.
  - More than 10 records collapse behind **Show N more**. More than 25 add a
    search box that narrows by name, id or flag.
  - The raw `via <server> · <tool>` line and the arguments (the generic card's
    whole body, ETag included) sit in a collapsed **Details** disclosure.
  - Approve & run, Cancel, the countdown, the seat badge and apply-all
    (subject to `critical_tool_no_session_approval`) are unchanged.
- **Resolved:** the header becomes a one-line outcome. The records move into
  Details, and the tool's result renders below as on every card. The outcomes:
  - `Applied · <title>`
  - `Not applied · <title>` (a failed run)
  - `Declined · <title>`
  - `Timed out — not applied · <title>`
  - `Outcome not recorded · <title>`
  - `Running · <title>` while executing
  - `<title> · ran without asking` for a notify record
- **Client-side check:** `parseApprovalCardData` (`history.ts`) re-checks the
  shape and drops a card that does not match, so the generic card renders
  instead of a half-rendered one. Every string renders as React text.

## Deviations

- **Applied, but the read-back didn't match.** Samantha's resolved states
  include this one, and fleet cannot tell it from the generic outcome: it
  knows only that the tool ran and whether it reported an error. A bundle tool
  that verifies its write says so in its result text, which renders under the
  one-line outcome. A per-tool outcome describer would be the way to make it
  a first-class state. Deferred.
- **`Not applied` carries no reason in the header.** The reason is the tool's
  failure text, shown in full directly below rather than truncated into the
  title.
- **Only the generic action card renders it.** A describer declared for an
  email tool is called and stored, but the email card keeps its own preview
  layout.
- **Notify-mode records are not described.** `RecordAction` runs after the
  call, with no staging step to hang a describer on.
- **The terminal client ignores `card`.** It still prints the frozen
  arguments before `/approve`, which remains the terminal's review record.
- **The card is captured once, at staging.** A card left pending for an hour
  still shows the values the describer read then. The approval executes the
  frozen arguments, not the card. A describer that reads current values
  should also make its write tool check them (an ETag, say) so a stale card
  cannot approve a write over a change it did not show.

## Deferred

- **Hot reload:** like every `agent_policy` value, the map is read at boot.
- **Adoption:** bundles adopt the key after the release that understands it
  (the manifest decoder is strict).
- **A live push of a late card:** the describer runs before the row exists,
  so there is nothing to push. A slower describer would need an asynchronous
  card update. The 5 s bound keeps staging prompt instead.
