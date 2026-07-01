# Dataset / table agent

Point an agent at a typed table and a goal; it works each row in the
background — research, enrich, classify — and writes results back as
**proposed** cell values a human reviews before they land. The "1000-row
agent" workflow (#514, ADR-0020) on fleet's governed run loop.

```
define dataset (columns + goal + pinned model)
  → import rows (CSV / JSON)
  → run: each pending row → agentcore.Run (sandbox + ceilings + redaction)
  → structured output validated against the output-column schema
  → row lands as PROPOSED   (free-form answers become a NOTE; never a cell)
  → review queue: approve (merge into cells) / retry / export CSV
```

## The contract

- **Typed columns**: `text | number | boolean`. *Input* columns carry your
  data; *output* columns are what the agent fills — they derive a strict
  draft-07 schema (`additionalProperties:false`, all output columns required).
- **Structured write-back only**: the row's final answer must validate against
  that schema. A non-conforming answer fails the row and is preserved as a
  `result_note` for review — free-form text never mutates a cell.
- **Review queue**: agent results are `proposed` until a human approves
  (per-row or bulk). Approve merges the proposed object into the row's cells.
- **Re-runnable**: retry failed rows in bulk, or reset any non-running row;
  approved cells persist until a new proposal is approved over them.
- **Governed per row**: every row is one `agent.Manager.RunTurn →
  agentcore.Run` — mandatory sandbox, per-run cost/token ceilings (the global
  per-turn ceilings bound each row), host-side credentials. Dataset
  `concurrency` (1..8, default 2) bounds parallel rows; the box-wide admission
  limiter still applies.
- **Pause / resume**: pausing cancels the run between rows (in-flight rows
  persist their outcome); a crash resets orphaned running state at boot so
  work resumes cleanly.

## Untrusted row data (the injection surface)

Cell values are attacker-influenceable (imported CSVs, scraped data). The
per-row prompt embeds them **as a compact JSON object explicitly labeled
untrusted** — never interpolated into instruction text — so a value cannot
terminate its string quoting or rewrite the goal. A successful injection is
further bounded by the sandbox, the ceilings, and the human review gate on
every write-back.

## Surfaces

- **API** (orchestrator, documented in `docs/openapi.yaml`):
  `POST/GET /datasets`, `GET/DELETE /datasets/{id}`,
  `GET/POST /datasets/{id}/rows` (JSON `{"rows":[…]}` or `text/csv`, header =
  input column names; ≤5000 rows / 16 MiB per request),
  `POST /datasets/{id}/run|pause|approve|rerun`, `GET /datasets/{id}/export`.
- **Web**: Operations Center → **Datasets** tab (create, CSV import, run
  controls, review/approve, retry, export).

## Honest scope (deferred)

- **No scheduling** — runs are on-demand (pair with a scheduled task hitting
  `POST /datasets/{id}/run` if needed); native cron datasets are a follow-on.
- **No per-row MCP connector selection** — rows run with the native tool set;
  the connector allowlist story follows the eval harness's.
- **No per-dataset budget ceiling** — each row is bounded by the global
  per-run ceilings; a dataset-level aggregate cap is a follow-on (per-row
  costs are recorded, so the data is already there).
- **No golden promotion** — promoting representative rows into #502 eval sets
  is a natural follow-on (`fleet eval capture` covers tasks/conversations
  today).
- **No cell editing in the UI** — cells change via import, approval, or
  re-run.
