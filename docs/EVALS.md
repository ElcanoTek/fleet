# Self-hosted evals & regression gating

fleet can replay "golden" prompts — captured from real runs — through its own
governed run loop and score the outputs, so a **model swap** or a
**persona/bundle edit** can be gated on *"did my known-good tasks get worse?"*
instead of shipped blind. Everything is self-hosted: goldens, rubrics, replays,
and the LLM-judge all run on your box against your configured providers. No
transcript ever leaves for a third-party grader. (Issue #502, ADR-0018.)

```
bundle evals/*.yaml ──▶ fleet eval run <set> ──▶ replay via agentcore.Run ──▶ scorers ──▶ gate (exit 0/1)
        ▲                                                                        │
        └────────── fleet eval capture (task / conversation) ◀──────────────────┴──▶ eval_runs (history)
```

## Eval sets: the `evals/` bundle contract

Eval definitions are **client content** (ADR-0006): they live in your external
bundle's `evals/` dir, one YAML file per set. The in-repo generic bundle ships
a template at [`config/default/evals/example.yaml`](../config/default/evals/example.yaml).

```yaml
set: weekly-reports            # default: file basename
description: Goldens for the weekly reporting workflows
threshold: 0.8                 # min pass fraction (absent = 1.0) — the gate
judge_model: "anthropic/claude-sonnet-4-6"   # default for llm_judge scorers
cases:
  - name: q3-revenue-summary
    prompt: "Summarize Q3 revenue drivers from the attached context …"
    model: "anthropic/claude-opus-4.8"   # PINNED — what a model swap compares
    persona: analyst                     # resolved from the LIVE bundle at replay
    expected: "…reference answer…"       # optional, strengthens the judge
    source: "task:2f9c…"                 # provenance (set by capture)
    scorers:
      - contains: "revenue"
      - regex: "(?i)q3"
      - equals: "exact text"             # whitespace-trimmed exact match
      - llm_judge:
          rubric: "Is the summary faithful and complete?"
          model: ""                      # default: set judge_model, then case model
          min_score: 0.7                 # pass bar on the 0..1 score (default 0.7)
```

Design point: the **model is pinned** per case (that is the variable a swap
eval compares), while the **persona/system-prompt content resolves from the
live bundle at replay time** — a bundle edit is exactly the change the eval is
meant to detect.

A malformed set is *excluded* (never half-run) and reported by
`fleet eval list` / as warnings on `run`.

## Scorers

| kind        | pass when                                   | notes |
|-------------|---------------------------------------------|-------|
| `contains`  | output contains the string                  | deterministic (`internal/scorers`) |
| `regex`     | Go regexp matches the output                | invalid pattern fails closed |
| `equals`    | output equals the string (whitespace-trimmed) | |
| `llm_judge` | judge `pass` AND `score ≥ min_score`        | verdict schema-validated; one corrective retry; judge *error* **fails** the scorer (fail-closed) |

A case passes only when **every** scorer passes; its score is the scorer mean
(deterministic scorers count 1.0/0.0). The set passes when
`passed/total ≥ threshold` — that is the CI gate.

The deterministic scorers are shared with the scheduled loop's exit conditions
(`internal/scheduledrun/loop.go` delegates to `internal/scorers`), so an eval
and a loop judge the same output identically.

## Running

```sh
fleet eval list                          # sets in the active bundle (+ problems)
fleet eval run <set>                     # replay + score + gate; exit 0 pass / 1 fail
fleet eval run <set> --json              # machine-readable result
fleet eval run <set> --no-db             # skip eval_runs persistence (CI without a DB)
fleet eval run <set> --temperature 0.3   # replay temperature (default 0)
fleet eval history [set] [--limit N]     # persisted runs, newest first
```

`fleet eval run` is an in-process replay: it builds the same engine `fleet
serve` boots (model resolver, sandbox pool, persona/prompt dirs), so it needs
what the server needs — model credentials (`OPENROUTER_API_KEY` or a manifest
`providers:` block) and a working rootless-podman sandbox. Replays use the
bundle selected by `FLEET_CLIENT_CONFIG_DIR` / `--bundle-path`.

Each run (unless `--no-db`) appends one immutable row to the orchestrator DB's
`eval_runs` table — set aggregate, per-case JSONB detail, cost, and a
**bundle fingerprint** (sha256 over manifest + prompts/personas/protocols/
skills/evals raw bytes) so two runs are an apples-to-apples model comparison
only when the fingerprint matches. The text report shows the delta against the
set's previous run.

## Capturing goldens

```sh
# From a scheduled task's latest run (prompt + pinned model + persona;
# the last assistant answer becomes `expected`):
fleet eval capture --task <uuid> --set weekly-reports

# From a chat conversation (first user message → prompt, last assistant → expected):
fleet eval capture --conversation <id> --user alice@example.com --set weekly-reports

# Options: --name my-case  --model <slug>  --rubric "…"  --out path|-
```

Capture appends to `<bundle>/evals/<set>.yaml` (creating it if needed) with a
single default `llm_judge` scorer — **review and tighten the scorers before
relying on the case as a gate.** Note the scheduled `logs` table upserts per
task, so only a task's *latest* run is capturable; capture soon after the run
you mean to bless.

## Gating a bundle repo's CI

fleet's own CI guards the harness; YOUR bundle repo gates its own edits
(the bundle is out-of-repo, ADR-0006). Recipe:

```yaml
# .github/workflows/evals.yml in your bundle repo
jobs:
  evals:
    runs-on: [self-hosted, fleet]   # needs rootless podman + model credentials
    steps:
      - uses: actions/checkout@v4
      - run: fleet eval run smoke --bundle-path "$PWD" --no-db
        env:
          OPENROUTER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
```

Exit 1 (pass fraction below threshold) fails the job and blocks the merge.
Gate a model swap the same way: edit the pinned `model:` in a branch and let
the eval decide.

## Honest scope (what this does NOT do yet)

- **No MCP tools in replays** — goldens replay with the native tool set
  (bash/python/files in the sandbox); MCP-dependent goldens are a follow-on.
- **No online scoring / canary** — production runs are not sampled or scored;
  no N% traffic comparison. The `eval_runs` schema is where those land.
- **No replay cache** — every `fleet eval run` pays real model spend; the
  bundle fingerprint is recorded for comparison, not yet used as a cache key.
- **No web UI** — CLI + `eval_runs` only.
- **Per-case knobs are minimal** — model + persona are pinned per case;
  temperature is per-run (`--temperature`), and `max_iterations`/network mode
  come from server config, not per case.
- **Chat capture is one-shot** — a multi-turn conversation captures its FIRST
  user message as the prompt (goldens replay as one-shot turns).
