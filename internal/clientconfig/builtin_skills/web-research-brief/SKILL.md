---
name: web-research-brief
description: Produce a decision-ready research brief from web sources — clarify the question, plan 3–5 targeted searches, triangulate every claim across at least two independent sources, separate fact from vendor marketing, cite every claim with its URL, and close with a confidence-rated summary plus open questions. Use it whenever the user asks you to research, compare, evaluate, or "find out about" something using the web.
---

# Web research brief

Produce a brief someone can act on, not a pile of links. Follow the steps in
order; do not start searching before step 2 is written down.

## Step 1 — pin the question

Restate the research question in one sentence, including the decision it
supports (e.g. "Should we adopt X for Y? — deciding by <constraint>"). If the
request is ambiguous on scope, timeframe, budget, or region, ask the user
before searching; otherwise state the assumptions you are making and proceed.

## Step 2 — plan 3–5 searches

Write the plan before executing it: 3–5 distinct queries that attack the
question from different angles — e.g. official docs/specs, independent
benchmarks or reviews, pricing pages, community experience (issues, forums),
and known failure modes ("X problems", "X vs Y migration"). One query per
angle; refine rather than multiply.

## Step 3 — gather and triangulate

- Execute the searches; open the most authoritative result per angle first
  (primary source > independent analysis > aggregator > vendor blog).
- **Every load-bearing claim needs ≥2 independent sources.** Two pages citing
  the same press release are one source. If you can find only one, keep the
  claim but mark it "single-source".
- Record the URL and publication date for everything you keep. Prefer sources
  from the last 12–18 months for anything fast-moving; flag older ones.
- Note conflicts explicitly — do not silently pick the number you like.

## Step 4 — separate fact from marketing

Label each finding as one of:

- **Fact** — verifiable, from documentation, changelogs, benchmarks you can
  inspect, or multiple independent reports.
- **Vendor claim** — from the seller's own materials and not independently
  confirmed. Report it as "vendor states…", never as established fact.
- **Opinion/anecdote** — individual experience; useful signal, weak evidence.

## Step 5 — write the brief

Structure the output exactly as:

1. **Question** — one line, plus assumptions made.
2. **Bottom line** — the answer/recommendation in 2–3 sentences, up front.
3. **Findings** — grouped by theme; each claim followed by its source URL(s)
   inline. Facts first, vendor claims clearly labeled.
4. **Conflicts & caveats** — where sources disagreed, and which you weighted
   and why.
5. **Confidence** — High / Medium / Low for the bottom line, with one sentence
   on why (source quality, recency, coverage gaps).
6. **Open questions** — what you could not verify and what search or expert
   would resolve each.

Keep the whole brief under about a page unless asked for depth; the appendix
of raw links belongs at the end, not inline.
