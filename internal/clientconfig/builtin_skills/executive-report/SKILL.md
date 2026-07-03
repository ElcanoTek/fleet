---
name: executive-report
description: Turn analysis results into a decision-ready executive report — lead with the answer and recommendation, quantify impact in business terms, cap visuals at one chart or table per point, push methodology to an appendix, use plain language, and end with explicit next-step asks. Use it when presenting analysis, findings, or a proposal to decision-makers rather than practitioners.
---

# Executive report

The reader has two minutes and a decision to make. Structure the report so the
first paragraph alone is enough to act on; everything after it is support.

## Step 1 — write the bottom line first

Open with a 2–4 sentence **Bottom line** block containing, in order:

1. The answer to the question that was asked.
2. Your recommendation (one concrete action).
3. The quantified stakes ("saves ~$40k/yr", "affects 12% of orders",
   "3 weeks of one engineer").

If you cannot fill in item 3, go back to the analysis — an unquantified
recommendation is an opinion. Ranges with stated assumptions are fine;
"significant impact" is not.

## Step 2 — build the body: one point, one exhibit

- Make 3–5 supporting points, each a short bold claim sentence followed by
  2–4 sentences of evidence.
- **At most one chart or table per point** — the one that proves the claim.
  If you need two exhibits, you have two points; split them. Cut any exhibit
  that merely decorates.
- Every number gets a comparison anchor (vs last quarter, vs target, vs the
  alternative) — a lone number carries no meaning.
- State counter-evidence and limitations honestly in one short "What would
  change this" paragraph; a report that hides its weaknesses gets re-litigated
  later.

## Step 3 — translate to plain language

- Replace jargon with what it means: not "p95 latency regressed 34%," but
  "the slowest 1-in-20 requests got a third slower — noticeable to users."
- No acronyms without expansion on first use; no method names (regression,
  cohort analysis) in the body — those live in the appendix.
- Prefer short declarative sentences. Read each paragraph and cut every word
  the decision does not need.

## Step 4 — end with explicit asks

Close with a **Next steps** section of concrete asks, each with an owner and
a date shape:

- "Approve X by <date> so <consequence of delay>."
- "Decide between option A (<cost/benefit>) and B (<cost/benefit>)."
- "No action needed; we will report again after <milestone>."

Vague endings ("we should consider…") are not allowed — every report ends in
a decision, a delegation, or an explicit "no action".

## Step 5 — appendix

Move to an appendix (never the body): methodology, data sources and their
dates, assumptions and exclusions, full tables, and sensitivity notes. Label
it so a skeptical reader can audit the bottom line without the appendix ever
interrupting the argument.

## Final check

- Does the first block alone let the reader decide? If not, rewrite it.
- Is every claim in the body quantified and anchored?
- Exhibit count ≤ point count?
- Would a reader outside your discipline understand every sentence?
