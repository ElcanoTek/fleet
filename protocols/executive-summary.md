# Executive Summary Protocol

Produce a weekly executive summary with historical trend analysis, forecasting, and client/campaign ranking from report emails delivered to a given mailbox.

## Usage

```
Run the executive summary protocol for emails sent to '<mailbox>' and send the report to '<recipient>'
```

**Example:**
```
Run the executive summary protocol for emails sent to 'raptive@victoria.elcanotek.com' and send the report to 'brad@elcanotek.com'
```

## Inputs

| Parameter | Source | Description |
|-----------|--------|-------------|
| mailbox | User task text | The `recipient_contains` address to search for report emails |
| recipient | User task text | Where to send the executive summary |
| lookback_days | Default: 120 | How far back to search for historical weekly reports |
| history_weeks | Default: 13 | Number of historical weekly data points to build |
| forecast_weeks | Default: 5 | Number of weeks to forecast forward |

## Step 1 — Discover weekly report snapshots

Build a series of weekly data points by searching for report emails:

- Use `mcp_email_search_emails` with `recipient_contains=<mailbox>`, `has_payload=true`, bounded day-slice queries
- Start from today and work backward in 7-day increments to find `history_weeks` weekly snapshots
- For each target week, search that day first, then the day before as fallback
- Each week point should have a complete set of SSP source reports (as defined by the workflow YAML)
- Download attachments for each selected week point

If a workflow YAML is provided, follow its `retrieval_and_selection` rules for SSP bundle requirements, sender/subject patterns, and fallback logic.

## Step 2 — Parse and compute metrics

- Load each week's attachments into pandas
- Apply SSP-specific column mappings for spend and margin fields
- If the workflow references `protocols/margin-calculation.yaml`, use it to compute weekly margin (client_share) from spend and curator margin fields
- Enforce exact week date bounds at the row level before aggregation — a selected file may cover more days than the target week
- Build a time series: one row per week with `week_end_date`, `weekly_margin_usd` (or `weekly_spend_usd` if margin protocol is not referenced)

Use ratio-of-sums for all derived metrics. Never drop rows due to unresolved revshare codes — retain them in totals and flag.

## Step 3 — Forecast

Using the historical weekly series, produce a `forecast_weeks`-week forward projection:

- **Baseline:** Blend of median (stability) and recency-weighted mean of recent points
- **Trend:** Only apply a trend adjustment if the data shows a persistent, material directional signal (most weeks trending the same direction with meaningful magnitude)
- **Default to stability:** If no clear trend signal exists, forecast flat from baseline
- **Confidence bands:** Widen with higher historical volatility; narrow with stable history
- **Confidence level:** High (stable, full history), Medium (moderate gaps or volatility), Low (sparse data or high volatility)
- **Floor:** Forecast should not go below 70% of baseline unless trend is confirmed

If the workflow provides a specific forecasting method (e.g., `method_v1`), follow its exact formulas.

## Step 4 — Current-week ranking

From the latest selected report only:

- Group by agency + brand (or campaign, depending on available fields)
- Rank by the primary metric (margin or spend) descending
- Show top 10 contributors
- Consolidate tiny contributors (below threshold) and unresolved deal names into an "Other/Other" bucket
- Parse agency/brand from deal names using tokenized SSP-aware rules; never map DSP names (TTD, DV360, etc.) as agency

## Step 5 — Visualize

Generate a weekly trend chart:

- Historical series as solid line with markers
- Forecast as dashed line with confidence band shading
- Render as PNG, embed inline in the email using `cid:weekly_chart`
- If chart generation fails, include a compact table fallback

## Step 6 — Compose and send

Build an HTML email following `protocols/email-style.yaml`.

**Sections (in order):**

1. **Executive Snapshot** — 2–4 sentences covering: latest weekly headline, historical growth direction, forecast confidence, top contributor this week
2. **Historical Weekly Trend** — inline chart image followed by the weekly data table (week label, value, WoW growth absolute and %)
3. **Forecast** — table with `forecast_weeks` rows plus confidence level and band width
4. **Top Contributors This Week** — ranked table from Step 4 with zebra-striped rows
5. **Source Attribution** — which reports were used, any coverage gaps

Attach audit CSVs:
- `selected_weekly_report_manifest.csv` — which emails/weeks were selected, date coverage, metric fields used
- `margin_audit_log.csv` (if margin protocol is used) — per-week reconciliation of margin math

Before sending, verify:
- All required sections present in order
- Historical points are spaced ~7 days apart and non-overlapping
- Forecast includes all `forecast_weeks` rows with numeric values (or explicit N/A)
- Chart is embedded inline (not as a regular attachment)
- No unresolved placeholders or template tokens
- Top contributor table has at most one Other/Other row

Set status to COMPLETE or COMPLETE_WITH_FLAGS based on whether all checks pass.
