# Optimization Protocol

Analyze recent campaign report emails for a given mailbox and produce actionable optimization recommendations.

## Usage

```
Run the optimization protocol for emails sent to '<mailbox>' and send recommendations to '<recipient>'
```

**Example:**
```
Run the optimization protocol for emails sent to 'twc.johndeere.elc00096@victoria.elcanotek.com' and send recommendations to 'brad@elcanotek.com'
```

## Inputs

| Parameter | Source | Description |
|-----------|--------|-------------|
| mailbox | User task text | The `recipient_contains` address to search for campaign reports |
| recipient | User task text | Where to send the optimization recommendations email |
| lookback_days | Default: 14 | How far back to search for report emails |

## Step 1 — Retrieve recent reports

Search for report emails delivered to the mailbox address:

- Use `mcp_email_search_emails` with `recipient_contains=<mailbox>`, `has_payload=true`, bounded date window (today back to lookback_days)
- Select up to 5 most recent emails with attachments
- Download all CSV/XLSX attachments from selected emails
- Record which reports were found and their date coverage in task_tracker

If fewer than 2 qualifying emails are found, note the gap and proceed with what's available.

## Step 2 — Load and explore the data

- Load every downloaded attachment into pandas
- Identify available columns and metrics (spend, impressions, clicks, conversions, CTR, CPA, etc.)
- Determine the campaign/deal structure (deal names, campaign names, advertiser tokens)
- **Derive channel** for each row from deal name, campaign name, or ad group name tokens:
  - Display: `DIS`, `_DIS_`, `-DIS-`, `Display`
  - OLV (Online Video): `OLV`, `_OLV`, `Video`
  - CTV (Connected TV): `CTV`, `_CTV_`, `Connected TV`
  - If a row matches none of these, label it "Other". OLV token wins over Display if both match.
- Print summary statistics — never print full dataframes

### Elcano deal separation (default behavior)

After loading data, classify every row as Elcano or non-Elcano using the Victoria persona's `elcano_deal_identification` patterns. By default:

- **Elcano deals** are the primary analysis set — Steps 3 and 4 (dimensional analysis and recommendations) run on Elcano deals only.
- **Non-Elcano deals** get a brief summary (total spend, impressions, deal count) in the email under a "Non-Elcano summary" line, but no dimensional breakdown or recommendations.
- The Topline Metrics table (Step 5) shows Elcano totals per channel, with a separate "Non-Elcano" total row for context.

The user MAY override this by asking to "include all deals" or "analyze non-Elcano deals too".

Use ratio-of-sums for all derived metrics:
- CTR = `100.0 * SUM(clicks) / NULLIF(SUM(impressions), 0)`
- CPA = `SUM(spend) / NULLIF(SUM(conversions), 0)`
- CPC = `SUM(spend) / NULLIF(SUM(clicks), 0)`
- ROAS = `SUM(revenue) / NULLIF(SUM(spend), 0)`

## Step 3 — Analyze dimensions

Explore as many of these dimensions as the data supports:

- **Inventory** — domains, apps, publishers, placements. Find top/bottom performers.
- **Temporal** — day of week, hour of day, date trends. Spot pacing issues or efficiency windows.
- **Geographic** — DMA, state, region. Identify high/low performing geos.
- **Device/technical** — device type, OS, browser. Flag any outliers.
- **Supply path** — SSP, exchange, deal type. Check for concentration risk.
- **Creative** — creative ID, format, size. Look for fatigue or standout performers.

For each dimension, identify:
- High-efficiency segments (strong metric ratios relative to peers)
- Low-efficiency segments consuming spend without proportional results
- Hidden gems (low spend, high efficiency)
- Concentration risks (>50% of spend in one bucket)

Do NOT assume a specific KPI target is known. Frame findings descriptively — report what the data shows across available metrics — rather than prescribing "good" or "bad" against an assumed target. The analysis in this step is goal-agnostic; goal-dependent interpretation happens in Step 4.

## Step 4 — Build recommendations

### Campaign goal context

The client's primary KPI or campaign goal may or may not be known. The task text, deal names, or campaign metadata may state it explicitly (e.g., "CPA target of $15", "awareness campaign"), or it may be entirely absent. Handle both cases:

**When the goal IS known** (stated in task text, deal metadata, or campaign brief):
- Optimize directly against that KPI. Frame recommendations in terms of that specific metric.
- Example: if the stated goal is CPA, lead with CPA-focused recs and quantify impact in CPA terms.

**When the goal is NOT known**:
- Do not invent or assume a specific KPI target. Instead, frame recommendations conditionally across the most likely goals given the data:
  - If the goal is **awareness/reach**: prioritize impressions, reach, frequency management, viewability, CPM efficiency
  - If the goal is **engagement**: prioritize CTR, video completion rate, interaction rate
  - If the goal is **conversions/performance**: prioritize CPA, CPC, ROAS, conversion rate
  - If the goal is **brand safety/quality**: prioritize viewability rates, brand-lift proxies, inventory quality
- Explicitly state that the goal was not provided and recommend the reader prioritize based on their actual objective.

In either case, each recommendation should note which goal(s) it aligns with so the reader can filter appropriately.

### Recommendation structure

Produce 3–7 specific, actionable recommendations. Each must include:

- **What to do** — be specific (which domains/geos/creatives, by how much)
- **Why** — what the data shows (with numbers)
- **Expected impact** — quantified where possible, framed conditionally by goal (e.g., "if the campaign goal is conversions, shifting $X from bottom 5 domains to top 5 could improve blended CPA by ~Y%")
- **Priority** — high / medium / low
- **Goal alignment** — which campaign goal(s) this recommendation serves

### Recommendation types

Recommendation types to consider, each framed by the applicable goal:

- **Blocklist / allowlist updates** (specific domains or apps) — applicable across all goals
- **Bid adjustments** on underperforming or outperforming segments — applicable across all goals
- **Pacing corrections** if spend is front- or back-loaded — applicable across all goals
- **Creative rotation** if fatigue signals are present — most relevant for awareness/engagement goals
- **Within-channel budget shifts** between geos, inventory segments, or supply paths — generally actionable

### Cross-channel budget reallocation

Recommendations that involve **moving spend between channels** (e.g., Display → OLV, or OLV → CTV) are fundamentally different from within-channel optimizations. These require:

- **Explicit caveat**: always include a note such as "This assumes the trader is able to reallocate budget across channels. If channel budgets are fixed or managed separately, focus on the within-channel optimizations above instead."
- Never present cross-channel reallocation as the default or only path. Always provide a within-channel alternative.
- If you cannot determine whether cross-channel reallocation is feasible, note the uncertainty — do not assume it is possible.

### Scientific rigor

Every recommendation must meet these standards:

- **Sample size**: flag if a segment has fewer than 1,000 impressions or 50 clicks — findings may not be statistically meaningful
- **Correlation ≠ causation**: avoid claiming a segment "drives" outcomes without supporting evidence. Use language like "is associated with" or "correlates with" unless the causal mechanism is clear
- **Comparative basis**: always compare segments to a meaningful baseline (e.g., campaign average, channel average), not in isolation
- **Magnitude before direction**: report the size of an effect before recommending action on it (e.g., "CTR is 0.35% vs. 0.12% campaign average — a 3× difference" rather than just "CTR is higher")
- **Uncertainty**: if data is noisy, sparse, or the lookback window is short, explicitly note reduced confidence and adjust priority downward

## Step 5 — Send the email

Compose an HTML email following `protocols/email-style.yaml` and send to the recipient.

**Sections:**
1. **Status** — COMPLETE or COMPLETE_WITH_FLAGS
2. **Data Coverage** — which reports were analyzed, date range, any gaps. List the channels found (e.g., Display, OLV, CTV) and row counts per channel.
3. **Topline Metrics** — show aggregate totals (spend, impressions, clicks, CTR, viewability) **broken out by channel**. Render as a small table with one row per channel plus a total row so the reader sees channel mix at a glance.
4. **Key Findings** — 3–5 bullet executive summary of what the data shows
5. **Recommendations** — the prioritized recommendation table
6. **Supporting Detail** — per-dimension highlights that back up the recommendations
7. **Quality Flags** — any data gaps, low-confidence findings, or coverage issues

Attach a CSV export of the deal/campaign-level summary data used for analysis.
