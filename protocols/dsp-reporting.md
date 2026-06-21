# DSP Reporting Protocol

Produce a standardized weekly DSP performance brief for a single client and
send it to the requested recipient(s). This is the "what happened last week"
report — distinct from `optimization.md`, which produces forward-looking
recommendations.

## Usage

```
Run the DSP reporting protocol for <client> (<ELC code>), mailbox=<addr>, recipient=<addr>, primary_kpi=<name + formula>
```

**Example:**
```
Run the DSP reporting protocol for Scotts (ELC00115), mailbox=scotts.elc00115@victoria.elcanotek.com, recipient=brad@elcanotek.com, primary_kpi=CPCV = spend / completed_views
```

This is a one-shot run. There is no chance to interview the user. Parse the
task text once, infer sensible defaults for anything missing, document the
guesses in Quality Flags, and proceed. Never stop to ask a clarifying
question.

## Inputs

| Parameter | Required | Default / Inference | Notes |
|-----------|----------|---------------------|-------|
| client_name | yes | Infer from task text or from the ELC code's deal naming | Display name (e.g., "Scotts", "Sun Bum") |
| elc_code | yes | Extract `ELC\d{5}` from task text; else from mailbox local-part | Used to isolate data |
| mailbox | yes | Infer from task text; else `mcp_email_search_emails` with `subject_keywords=<client>` | `recipient_contains` for the inbound email tool |
| search_filter | no | Most recent matching email in the discovery window | Sender domain or subject keyword if provided |
| reporting_window | no | Last full Mon–Sun relative to today | Date range, inclusive |
| isolate_filter | no | `Deal Name contains <elc_code>` | Override only if user specifies |
| breakout_by | no | Channel | One of: Channel, SSP, Deal, Funnel stage |
| primary_kpi | yes | If absent: pick from data — CPCV when completed_views present, else CPA when conversions present, else CPM. Flag the inference. | Name + formula |
| wow_metrics | no | spend, impressions, primary_kpi | Which metrics go in the WoW block |
| pacing_target | no | `none` | Monthly budget (e.g., `$6,500/month`) or the literal `none` |
| recipient | yes | If absent: do NOT guess. Build the report, save the rendered HTML to the scratch dir, and skip `mcp_sendgrid_send_email`. | Email address(es) |
| special_notes | no | — | Decimal precision, dedup, flags, etc. |

### Inference policy

- Convert relative dates in the task text to absolute ISO dates before doing anything else.
- Pull `ELC\d{5}` codes with a regex; the first match wins.
- If `client_name` is missing but `elc_code` is present, derive it from the most common advertiser/brand token in the report's deal names.
- Record every inferred value as a Quality Flag entry (e.g., `inferred_primary_kpi=CPCV`, `inferred_reporting_window=2026-05-12..2026-05-18`).
- You **MUST NOT** probe the dataset to discover an operator-supplied input that is not a property of the data — a `pacing_target` / monthly budget, an attribution rule, or a recipient. These come from the task text, not the report. If such an input is absent, apply its default (`pacing_target` → `none`, so omit Step 5) and proceed; do **not** run exploratory `run_python` queries hunting for it. (Picking `primary_kpi` from the columns that *are* present per the Inputs table is the one allowed inference — that reads what the data has, it does not search for a value the data lacks.)
- If a **required** input cannot be inferred (most commonly `recipient`), still produce the full report and save the rendered HTML + supporting CSV to the scratch dir. Skip the send, finish with `COMPLETE_WITH_FLAGS` and a `missing_recipient` flag naming the on-disk artifacts. Never send to a guessed address.

## Step 1 — Retrieve the report email

- Use `mcp_email_find_latest_report` when the sender/subject pattern is known — one call walks back day-by-day from today to the first qualifying drop. Pass the mailbox as `recipient_contains` and `has_payload=true`.
- For ad-hoc lookups use `mcp_email_search_emails` with `recipient_contains=<mailbox>`, the inferred subject/sender filter, `has_payload=true`, and a bounded date window covering both the reporting window and the prior equivalent window (for WoW).
- Pick the most recent qualifying email. If the user said "use today's", pick today's drop and emit a `no_report_today` flag if none exists rather than silently substituting an older file.
- Download the CSV/XLSX attachment(s) with `mcp_email_download_attachment`. Record file name(s) and report-coverage date range in `task_tracker`.

Verify the data's actual date range after download — `received_at` is a discovery index, not coverage proof. If the data's `data_max` ≠ the expected end of the reporting window, step the window and re-check; accept within 2 adjustments or continue with a `period_overlap_detected` flag.

If no qualifying email is found at all, deliver a minimal report email stating that, with status `COMPLETE_WITH_FLAGS` and a `no_qualifying_email` flag. Never fabricate data from older reports.

## Step 2 — Load, filter, and explore

- Load every attachment into pandas via `run_python`. For `.xlsx`, inspect sheet layout before reading the full workbook.
- Apply the `isolate_filter` (default: deal-name contains the ELC code).
- **Derive channel** for each row using the same token rules as `optimization.md`:
  - Display: `DIS`, `_DIS_`, `-DIS-`, `Display`
  - OLV: `OLV`, `_OLV`, `Video`
  - CTV: `CTV`, `_CTV_`, `Connected TV`
  - Else: `Other`. OLV wins over Display on collisions.
- Auto-detect available metric columns: spend / revenue, impressions, viewable impressions, clicks, completed views, conversions / detail page views, purchases, etc.
- Print summary statistics only — never dump full dataframes.

Use ratio-of-sums for every derived metric:
- CTR = `100.0 * SUM(clicks) / NULLIF(SUM(impressions), 0)`
- CPM = `1000.0 * SUM(spend) / NULLIF(SUM(impressions), 0)`
- CPC = `SUM(spend) / NULLIF(SUM(clicks), 0)`
- CPCV = `SUM(spend) / NULLIF(SUM(completed_views), 0)`
- CPA = `SUM(spend) / NULLIF(SUM(conversions), 0)`
- CPDPV = `SUM(spend) / NULLIF(SUM(detail_page_views), 0)`
- CPP = `SUM(spend) / NULLIF(SUM(purchases), 0)`
- DPVR = `100.0 * SUM(detail_page_views) / NULLIF(SUM(impressions), 0)`
- VCR = `100.0 * SUM(completed_views) / NULLIF(SUM(video_starts), 0)`
- ROAS = `SUM(revenue) / NULLIF(SUM(spend), 0)`

If the chosen primary KPI formula references a column the data doesn't have, fall back to the next available KPI from the inference list (Step 0 table) and add a `primary_kpi_fallback` flag.

## Step 3 — Build the snapshot table

Aggregate the current reporting window grouped by `breakout_by` (default: Channel). Columns: every metric required to compute the primary KPI plus the KPI itself. One row per group plus a "Total" row.

Decimal precision:
- Currency: 2 decimals (`$1,234.56`) unless a sub-cent KPI is in play (CPCV often runs `$0.0157`); for sub-dollar values keep up to 4 decimals.
- Rates (CTR, DPVR, VCR): 2 decimals with `%` suffix unless `special_notes` overrides.
- Counts: thousands-separated integers.

## Step 4 — WoW comparison

Compute the same aggregation for the prior equivalent window (same length, ending the day before the current window starts). For each metric in `wow_metrics`:

- Show current value, prior value, absolute delta, % delta.
- Color: green for improvement, red for regression, gray for ≤ ±1% drift.
- "Improvement" is metric-aware: lower is better for CPCV / CPA / CPM; higher is better for spend (assuming a pacing goal exists), impressions, clicks, CTR, DPVR, VCR, completed views, conversions.

If prior-window data is missing or zero, label that metric `N/A — first full week` instead of computing infinite deltas.

## Step 5 — Pacing steer (conditional)

Only if `pacing_target` is provided and not `none`:

- MTD spend = SUM(spend) within the data's date range, capped at today.
- Target MTD = `(monthly_budget / days_in_month) * days_elapsed_in_month`.
- Pacing % = `MTD spend / target MTD spend`.
- Status: `On pace` (95–105%), `Under-pacing` (<95%), `Over-pacing` (>105%).
- Render as a `status_bar` plus one short paragraph under a "Pacing" body block stating MTD spend, target, and percentage.
- If under/over by more than 10%, also surface a `risk_callout` or `opportunity_callout`.

If `pacing_target` is `none`, omit the section entirely — no placeholder.

## Step 6 — Daily spend section (final section)

Always include this section. It renders **last** — after Notes & Quality Flags — so the day-by-day spend trail anchors the bottom of the email.

Render two tiers, in order:

**6a. DSP spend by day (total).** A single `data_table` keyed by date showing the whole DSP rolled up — all channels combined. Columns: **Date | Impressions | CPM | Spend**. One row per day, ordered ascending, plus a "Total" row. Kicker: `DAILY SPEND — DSP TOTAL`.

**6b. Per-channel daily spend.** For each channel present in the current reporting window, render a separate `data_table` keyed by date with the same columns. Use the channel name as the kicker (e.g., `DAILY SPEND — CTV`). Include a "Total" row per channel.

If only one channel is present, render 6a only — skip 6b to avoid duplicating the same table.

Computation (both tiers):
- Daily spend = `SUM(spend)` for that (date[, channel]) cell
- Daily impressions = `SUM(impressions)`
- Daily CPM = `1000 * daily_spend / daily_impressions` (recomputed per row, not averaged)

### Coverage rule — one row per day in the reporting window

The daily spend tables MUST contain one row for every calendar day in the
reporting window — not just every day the data happens to cover. A seven-day
Mon–Sun window means seven data rows (plus the Total row) even when the
qualifying email's attachment only covers part of it.

Before composing the email:

1. Enumerate the expected dates
   (`pd.date_range(window_start, window_end, freq="D")`) and diff against the
   dates actually present in the daily aggregate.
2. For any missing day, the agent MUST take these actions in order:
   1. Search the mailbox for an earlier qualifying report drop whose data
      window covers the gap (DV360 / SSP report drops typically arrive
      every Mon/Thu; the file's data window ends the day before the drop
      date). Download with `mcp_email_download_attachment` and re-aggregate.
   2. If no earlier drop covers the gap, emit the row with
      `"N/A — no source report"` in the Impressions / CPM / Spend cells
      and add a `daily_spend_missing_days=<comma-separated ISO dates>`
      Quality Flag.
3. The agent MUST NOT silently omit a row for a missing day and MUST NOT
   claim a gap was "filled in" unless step 2.1 actually produced rows
   covering those dates.

Apply the same rule to 6b: every channel-by-date cell in the channel × window
grid MUST appear, with explicit `N/A` markers where the available sources
can't cover that channel-day.

## Step 7 — Compose and send the email

Compose HTML by concatenating components from `protocols/email-style.yaml`. Theme: `victoria` unless the task text specifies otherwise. Subject: `<Client Name> Performance Report (<ELC code>)`.

**Section order:**

1. `header` + `status_bar` — kicker = `WEEKLY REPORT`, status = reporting window
2. **Executive Summary** — `body_text`, 2–4 sentences (volume direction, KPI direction, notable pacing flag)
3. **Snapshot Analysis** — Step 3 table
4. **WoW Comparison** — Step 4 block, with a sub-kicker noting the prior window dates
5. **Pacing** — Step 5 block (omitted if `pacing_target = none`)
6. **Notes & Quality Flags** — `closing` body with data gaps, inferred inputs, missing columns, or assumptions made
7. **Daily Spend** — Step 6 tables (6a DSP total, then 6b per-channel). Always last.
8. `footer` + `shell_close`

Validate then send:

1. Call `mcp_sendgrid_validate_email_content` on the rendered HTML. Fix any errors it reports.
2. Call `confirm_audit` listing the `mcp_sendgrid_send_email` action in `critical_actions_being_unblocked`. The DSP reporting protocol sends one email per run, so this is a single-action audit envelope.
3. Call `mcp_sendgrid_send_email` with `content_type="text/html"`, the resolved recipient(s), and the CSV row-level data attached so the reader can drill in.

Do **not** preview-and-wait. The chat version of this protocol calls `preview_email` and waits for the user to say "send" — that step is removed here because cutlass is one-shot. The audit gate (`confirm_audit`) is the only safety check before send.

## What this protocol is NOT

- Not a recommendations engine — use `protocols/optimization.md` for forward-looking advice.
- Not a tracking-workbook reader — use `protocols/tracking-doc.md` for client-curated `.xlsx` files.
- Not multi-client — one client per run. If the task text names multiple clients, run the protocol for the first one and flag `multi_client_requested` so the operator knows to schedule the others.
