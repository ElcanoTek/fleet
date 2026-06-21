# Health Check Protocol

Produce a weekly campaign health scan from SSP report emails delivered to a given mailbox.

## Usage

```
Run the health check protocol for emails sent to '<mailbox>' and send the report to '<recipient>'
```

**Example:**
```
Run the health check protocol for emails sent to 'omnicom.supply@victoria.elcanotek.com' and send the report to 'casey@omnicom.com'
```

## Inputs

| Parameter | Source | Description |
|-----------|--------|-------------|
| mailbox | User task text | The `recipient_contains` address to search for SSP report emails |
| recipient | User task text | Where to send the health check report |
| lookback_days | Default: 7 | Rolling window for report discovery |

## Step 1 — Discover and retrieve SSP reports

Search for report emails delivered to the mailbox:

- Use `mcp_email_search_emails` with `recipient_contains=<mailbox>`, `has_payload=true`, bounded date window (rolling last `lookback_days`)
- Identify SSP sources by sender domain and subject patterns (Index Exchange, Magnite, OpenX, PubMatic, etc.)
- For each SSP, select the most recent qualifying email with downloadable attachments (CSV/XLSX)
- Download all selected attachments
- Record selected sources, message IDs, and attachment names in task_tracker

If a workflow YAML is provided alongside this protocol, follow its `scope`, `retrieval_and_selection`, and `non_negotiables` for SSP-specific filtering, deal name patterns, and candidate ranking.

## Step 2 — Parse and normalize

- Load each downloaded attachment into pandas
- Normalize column names using SSP-specific mappings (deal name, spend, impressions, CPM, etc.)
- Filter rows to in-scope deals using the workflow's `deal_name_patterns` if provided, or include all rows otherwise
- Derive advertiser/campaign labels from deal names
- Drop subtotal/total rows
- Enforce SSP-specific spend column mappings (e.g., Magnite = "Buyer Spend", OpenX = "Demand Partner Spend in USD")

Use ratio-of-sums for derived metrics:
- Blended CPM = `spend * 1000 / NULLIF(impressions, 0)`
- Never average CPMs across rows — only weighted blended CPMs are valid for rollups

## Step 3 — Analyze

- Aggregate daily spend by advertiser, market, and format where available
- Compute period totals across all in-scope advertisers
- Flag pacing anomalies: any day where spend drops below 80% of the period daily average
- For flagged days, annotate with a warning marker and note likely contributing factors (use "observed" or "likely contributor" language — no causal claims)
- If the workflow references `protocols/margin-calculation.yaml`, apply margin calculations

## Step 4 — Compose the report

Build an HTML email following `protocols/email-style.yaml`.

**Sections (in order):**

1. **Opening context** — weekly performance update, scope, total spend across all advertisers, any data completeness notes
2. **Risk/flag callout** (only if pacing alerts exist) — highlighted warning box with flagged dates and contributing factors
3. **Advertiser sections** — one section per detected advertiser, each with:
   - Daily spend tables (columns: Date, Impressions, Spend, CPM)
   - Short interpretation paragraph under each table
   - Market/format subsections where deal naming supports it
4. **Outlook & Next Steps** — 2–4 concise action-oriented paragraphs with bold lead-ins
5. **Quality Flags** — any data gaps, partial coverage, or source issues (internal transparency)

Attach audit CSVs:
- `selected_source_evidence.csv` — which emails/attachments were selected and why
- `health_check_audit.csv` — deal-level rows with normalized spend used in final math

## Step 5 — Validate and send

Before sending, verify:
- All in-scope advertisers are represented
- Daily table totals reconcile to source attachment totals (within $1.00 tolerance)
- No raw operational terms in client-facing content (no SSP tool names, no retrieval mechanics)
- Currency formatted as `$#,##0.00`, impressions as grouped integers
- Flagged days are annotated; unflagged days are clean

If any check fails, set status to COMPLETE_WITH_FLAGS and document in Quality Flags section.

Send via `mcp_sendgrid_send_email` with `content_type="text/html"`. If the workflow specifies prohibited keywords (e.g., "margin" for certain clients), scan the email body and attachment headers before sending.
