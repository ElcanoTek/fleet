# Runtime date window (#1026)

Per-turn `runtime_today` so a persistent chat cannot stay anchored to
yesterday's mailbox search bounds.

## What shipped

Two independent defects in #1026 combined: the agent reused the previous
turn's `date_from`/`date_to` after UTC midnight, and an empty exact
sender/subject search was treated as proof a report was missing. Fleet is
an engine — OpenX-specific discovery, coverage-date parsing, and the
pre-send source-coverage ledger belong in the client bundle (reporting
skill + mailbox MCP). This change makes the date signal **harder to
ignore** and annotates the two failure modes the engine can see.

1. **Structured `runtime_today` on every run.** `agentcore.Run` appends a
   trailing user message (`RuntimeDateTurnSuffix`) with `runtime_today` and
   a 3-day `freshness_window` (`today-2d .. today`). Interactive and
   scheduled modes share this path. The suffix is model-facing only — it is
   not persisted as the user's typed text.
2. **Day-granular system-prompt date, strengthened.** The existing
   `runtimeDateContext` block now also names `runtime_today` and
   `freshness_window`, still at day precision (one cache miss per UTC
   midnight; see [PROMPT-CACHE-CONTRACT.md](PROMPT-CACHE-CONTRACT.md)).
3. **Stale-window annotation on MCP results.** After a mailbox/search tool
   returns, if `date_to` / `on_or_before` / `end_date` / `before` / `until`
   is a UTC day before `runtime_today`, the result gains a
   `[fleet date-window]` note telling the model to re-run through today
   unless the user asked for a historical range.
4. **Empty exact search is not absence.** A mailbox search tool
   (`search_emails`, `find_latest_report`, or any `*search*` call that
   passed sender/subject/recipient filters) that returns `matches_found=0`
   gains a `[fleet search]` note requiring recipient, sender-domain, and
   fuzzy fallbacks before declaring a source missing.

## Deviations / honest scope

- **No argument rewriting.** Hooks cannot rewrite tool input (ADR-0038),
  and injecting an undeclared `runtime_today` field into MCP args would
  fail `additionalProperties: false` schemas. The engine restates the date
  in the message tail and annotates stale results instead of mutating the
  call.
- **Not a hard send gate.** Blocking `send_email` until a source-coverage
  ledger is complete is client-workflow policy. The engine does not know
  which SSPs a report requires.
- **Not mailbox-index repair.** Subject punctuation (`|`), sender
  normalization, and `has_payload` false negatives are search-reliability
  bugs in the mailbox MCP (elcano-config / peer bundles). The engine only
  refuses to treat an empty exact hit as authoritative.
- **Scheduled system prompt stays bake-time.** `scheduledrun` composes the
  base prompt at process start. Putting `runtime_today` there would go
  stale until restart; the per-run tail covers scheduled tasks.

## Tests

- Date rollover: prior turn August 12, runtime August 13 → suffix and
  freshness window end on the 13th.
- Stale `date_to` / `on_or_before` annotation; current-window hits
  unchanged.
- Empty exact search with subject punctuation still parsed and flagged.
- `agentcore.Run` injects today's `runtime_today`, not yesterday's.
- `mcpTool.Run` surfaces both notes on a stale empty mailbox search.
