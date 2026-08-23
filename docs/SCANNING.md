# The scanning stack: who checks what, and what actually gates

Design note for the change that stopped treating "add a scanner" as strictly
better and gave each tool the job it is actually good at. Companion to
[`CODEQL.md`](CODEQL.md) (why default setup was replaced, and why CodeQL now runs
security queries only) and [`TESTING.md`](TESTING.md) (the rest of the ladder).

## The stack

| tool | scope | speed | gates? | where results appear |
| --- | --- | --- | --- | --- |
| `golangci-lint` (incl. `gosec`) | Go lint + Go SAST patterns | ~30s | **blocks** (`ci-gate`) | job log |
| `oxlint` + `tsc` | web tier lint + types | ~5s | **blocks** (`ci-gate`) | job log |
| **`ruff`** | **Python lint** | **~1s** | **blocks** (`ci-gate`) | job log |
| **`actionlint`** | **workflow YAML: `${{ }}` expressions, contexts, `needs`/`runs-on`/cron, + shellcheck over every `run:`** | **~2s** | **blocks** (`ci-gate`) | job log |
| **`shellcheck`** | **the 18 tracked `*.sh` files (~6.2k lines) — the deploy path** | **~2s** | **blocks** (`ci-gate`) | job log |
| `govulncheck` | Go dependency CVEs (called symbols) | ~30s | **blocks** (`ci-gate`) | job log + Security tab |
| `grype` | sandbox image CVEs (fixable **CRITICAL + HIGH**, **RPMs only**) | ~1m | **blocks** (`ci-gate`) | job log + Security tab |
| `gitleaks` | secrets (on `main` and `dev` — the two branches with a CI lane) | ~10s | **blocks** (`ci-gate`) | job log |
| **`npm audit`** | npm dependency CVEs (web + rampart-service) | ~5s | **blocks** (`ci-gate`) | job log |
| CodeQL | **interprocedural taint / `security-extended`** | ~2m | **blocks** on an unwaived High-band finding (`ci-gate`/`Dev gate` via workflow_call) | job log + Security tab |
| **Semgrep** | **Go/JS/Python SAST + Actions supply chain** | ~40s | **blocks** on any unsuppressed finding (`ci-gate`/`Dev gate` via workflow_call) | job log + artifact |

Four things were added here (**ruff**, **Semgrep**, then **actionlint** and
**shellcheck**) and one was narrowed (**CodeQL**, to security queries only).

**Why actionlint and shellcheck, given Semgrep already runs `p/github-actions`.**
The overlap is one axis wide: Semgrep's Actions pack is a *security* rule set —
mutable action tags, template injection, `pull_request_target` misuse — and
CodeQL's `actions` language is likewise security-query-only. Neither parses
`${{ }}` expressions and neither shellchecks a `run:` block. That mattered here
concretely: the census below found `run: "$GITHUB_WORKSPACE/scripts/..."` in both
CI lanes passing its path to the shell **unquoted**, because the YAML parser
consumes the quotes — a bug the comment directly above the line showed the author
believed they had avoided. And bash was the only language in the repo with no
linter at all, while being the language `fleet update` and `fleet bootstrap`
actually execute on an operator's box. Both gates started at **zero findings**;
the 5 actionlint and 3 shellcheck items were fixed or annotated with reasons
before the gate went in, because a gate switched on over a backlog is a gate
people learn to scroll past.

**Read "blocks" with one caveat, and it is a big one.** Every lane above reaches
its branch's aggregate gate job — but a gate job only *blocks a merge* where it
is a **required status check**. On `main` it is (`CI gate`). On `dev` the ruleset
requires no status checks at all, so `Dev gate` is red-but-not-required there.
The "gates?" column describes the wiring, which is real; the enforcement half is
branch-dependent. See ["Known gaps"](#known-gaps-deliberately-not-closed-here).

## Why each tool is where it is

**The design rule: one owner per job.** A second tool over ground an existing
blocking gate already covers does not add safety — it adds a queue of duplicate
findings, and a scanner whose output is mostly already-adjudicated noise trains
people to close the tab. Every placement below follows from that.

### ruff owns Python lint (new, blocking)

fleet ships 13 Python files — the sandbox FileOp helper, the python bridge, the
bento-slides and data-profiler skill scripts, MCP test servers, icon/doc
generators — and **nothing linted any of them.** Go had `golangci-lint`, the web
tier had `oxlint`, Python had neither. Its only coverage was whatever CodeQL's
code-quality suite happened to notice, at ~40s and with no autofix.

That gap was real: CodeQL's quality suite found 28 Python issues, its single
largest contribution anywhere. ruff finds the same class in under a second, with
autofix, and now blocks.

The rule selection is measured, and `ruff.toml` records the numbers. The
default rules (`E4,E7,E9,F`) found 3 real findings, fixed on day one. The
**`B`, `SIM` and `S` (bandit) families were then measured (21 findings), all 21
fixed, and the families enabled**: both `zip()` sites got `strict=True` (each
provably equal-length), the unclosed `NamedTemporaryFile` moved into its
`with`, the deliberate best-effort `try/except-pass` sites became explicit
`contextlib.suppress` with the intent stated at each, and the one subprocess
launch carries a reasoned `# noqa: S603` (argv is `sys.executable` plus
internal literals — mutation-tested: stripping the noqa re-fires the rule).
The pure-style tiers stay off: they are ~330 findings of `%`-format and
line-length churn with no correctness content.

Three real findings were fixed to make the gate clean on day one, so a new
violation is a regression rather than noise in a backlog:

- `internal/mcp/testdata/dummy_server.py` — unused `import os`.
- `bento_doc.py` — **a byte-identical duplicate `has_guard` definition.** Two
  copies, one call site; the second silently shadowed the first. Dead code, and
  the only finding here that was arguably a latent bug.
- `bento_pdf.py` — a lambda assigned to a name (`E731`), rewritten as a `def`.

One narrowing is worth naming, because the rule set is otherwise uniform across
all 13 files: `ruff.toml`'s `[lint.per-file-ignores]` waives **`F401` (unused
import) for `internal/mcp/testdata/*.py` and `cmd/fleet/testdata/*.py`**. Those
are deliberately minimal MCP stand-ins that exist to be spawned and to misbehave
in specific ways, so an import that nothing uses can be the point of the
fixture. Nothing else is waived anywhere, and the waiver is per-path and
per-rule — `F` still bites everywhere else in those directories.

`ruff format --check` is **also gated** (CI and `make lint`): the whole tree
was ruff-formatted in one dedicated commit (9 files, ~3.7k lines, validated
against the full Go suite — the bento/fileops golden tests exercise these
scripts), so the gate started clean and a failure means one new file.

### CodeQL owns interprocedural taint (narrowed, gates on the High band)

CodeQL is the only tool in this stack that does cross-function dataflow, and that
is exactly the shape of fleet's headline invariants: *a credential must not reach
a log sink, the model context, or the sandbox.* `go/clear-text-logging` is
literally that query. Nothing else here can express it.

So CodeQL keeps its security queries and gives up everything else — the quality
suite duplicated `golangci-lint`/`oxlint` for Go and JS, and ruff is a better fit
for Python. Full reasoning and measurements in [`CODEQL.md`](CODEQL.md).

It runs the **`security-extended`** suite — the broader security set — and a
`Fail on findings` step fails the job on an unwaived finding in the **High band**
(`security-severity >= 7.0`; for a rule that publishes no security-severity, the
fallback is SARIF level `error`/`warning`). So a red `Analyze (…)` check means
the *code* has a problem rather than just "the scanner broke", which is the
distinction the Go toolchain break survived weeks inside of. Findings below the
band are **advisory**: printed in the job log and step summary, uploaded to the
Security tab, not blocking. The threshold and the reasoning behind it are
[ADR-0048](adr/0048-codeql-severity-gating.md).

**The threshold used to be "any finding", and correcting that is the most
instructive thing in this document.** The any-finding gate was armed on a
measured zero — Dev CI run 525, across all four languages. That run was a
`pull_request` event, and on `pull_request` events the CodeQL action runs
**diff-informed**: it builds the full database and evaluates every query, then
reports only results whose location falls inside the PR's diff. Run 525's own
log says both halves out loud — `Persisted 204 diff range(s) across 43 file(s)`
and `file coverage information is only enabled when analyzing the default branch
and protected branches`. The zero measured the **diff**, not the tree.

The first full-tree evaluation was therefore the **push** that merged that work:
Dev CI run 527, which reported **38 Go and 17 javascript-typescript findings** and
turned `Dev gate` red — with no PR-shaped way out, because a PR into `dev` is
scanned diff-informed and comes back green while `dev` itself stays red.

The generalisable rule, worth carrying to any scanner that supports diff-scoped
analysis: **a PR-event CodeQL run certifies a diff, not a tree.** Any claim of
the form "the scanners are green, therefore the tree is clean" that rests on a
`pull_request` run is unsound. Tree-wide verdicts come from the push and
scheduled runs.

Of those 55, **four were reachable and were fixed in code, not waived**: an
unsanitized `task.Prompt` in the task-create log (its update-path twin was
already wrapped in `logSafe`), the raw pre-validation client attachment path
logged on the two branches where the containment guard had just *failed*, the
client-echoed attachment `Name` on the `/chat` path, and an Ed25519 private key
written to a predictable world-writable temp path at `0644` in
`web/e2e/test-auth-key.ts`. The remaining 51 are false positives in fleet's
threat model, and severity alone does not separate them — `go/request-forgery`
is 9.1 and fires on the deliberate `@url` fetch tool behind `internal/netguard`'s
resolve-then-dial SSRF guard; `go/weak-sensitive-data-hashing` is 7.5 and fires
on SHA-256 used as a lookup index over a 32-byte `crypto/rand` token, which is
the recommended construction.

Those 51 live in **`.github/codeql-accepted-findings.json`**, a register of
accepted `(rule, file)` pairs each carrying a mandatory written reason. It is
per-**file**, not per-rule, and that is the whole point of preferring it to a
`query-filters` exclude: excluding `go/request-forgery` would switch a
security-severity 9.1 query off for the entire repository, whereas a register
entry waives it in the two files that were read and leaves the query live
everywhere else. The register is the **only** waiver route that works here: an
in-source `// codeql[rule-id]` comment does **not** waive with this pipeline —
measured on PR #1249: three forms were tried (the `packs:` input, `packs:` with the additive `+` prefix, and an inline `config:` combining security-extended with codeql/go-queries' `AlertSuppression.ql`) and in every case the uploaded SARIF carried no `suppressions` on the annotated result. A deliberately-waived Security-tab alert is closed by a one-time
human dismissal there. Widening the register is a security decision
that appears in the PR diff, and `scripts/check_codeql_register_test.go` fails
`make test` on an entry naming a file that does not exist, a missing reason, or a
register that `codeql.yml` has stopped referencing.

**One classifier, two consumers.** `.github/codeql-gate.jq` does the banding and
the waiver lookup, and both the summary step and the gate step run it via
`jq -f` — two copies of a SARIF filter is two copies that can disagree about
what "blocking" means, and the report disagreeing with the gate is worse than
either being wrong alone. The job log prints **three tiers** from that one
classification: BLOCKING, ACCEPTED (by name — a waiver nobody re-reads is worse
than no waiver) and ADVISORY.

**It fails closed.** A missing register, a missing filter file, SARIF that will
not parse, and — the subtle one — findings present with **zero rule metadata
resolved** all fail the job rather than reporting clean. That last is a vacuity
check with a real provenance: CodeQL puts query metadata in
`tool.extensions[].rules[]`, not `tool.driver.rules[]`, and a first cut of the
filter read only the driver, resolved nothing, scored every finding at
security-severity 0 and reported "0 blocking" over a tree holding findings.

Getting the extended suite adopted was itself a fix, not a rubber stamp: the one
`actions`-language finding was `actions/untrusted-checkout/medium` on
`build-sandbox-image.yml`'s `fleet_ref`-fed checkout. Rather than waive it (the
`actions` language has no `AlertSuppression.ql`, so there is no in-code waiver
anyway), the workflow now validates `fleet_ref` before checking out — a fork-PR
ref would put fork-controlled code into a workflow that runs the checked-out
build script — and the identical hardening went into
`publish-sandbox-image.yml`, the *unflagged* twin that holds `packages: write`
and only escaped the (name-heuristic) query because its plumbing was named
differently.

**It is an ALLOW-LIST, and an earlier revision of this document described the
deny-list it replaced.** Worth correcting rather than quietly updating, because
the deny-list (`refs/pull/*|pull/*|-*`) had two holes that a reader
re-implementing "refuse `refs/pull/*`" elsewhere would inherit:

1. **`GITHUB_OUTPUT` newline injection.** A `workflow_call` string input may
   contain newlines, so `fleet_ref: "main\nresolved=refs/pull/1/head"` matched no
   deny pattern (it starts `main`), exited 0, and emitted **two** `resolved=`
   lines — last-wins handed the attacker the ref. The same primitive forges any
   step output.
2. **A bare commit SHA.** "Every ref in this repo is collaborator-written except
   `refs/pull/*`" is true of *named refs* and false of reachable *commits*:
   GitHub keeps fork-PR commits in the base repo's object store and
   `actions/checkout` will fetch a bare SHA happily.

The shipped form admits only `[A-Za-z0-9._/-]` (so no newline can carry a second
assignment), then refuses `-*`, `*..*`, `*//*`, `refs/pull/*` and a bare hex SHA. Details in [`CODEQL.md`](CODEQL.md).

### Semgrep owns fast multi-language SAST + Actions supply chain (new, blocking)

Semgrep is the opposite trade from CodeQL: seconds instead of minutes, no
database build, findings straight to stdout, rules cheap to write. That makes it
the right fit for an agent-driven loop.

All four packs run — `p/github-actions`, `p/golang`, `p/javascript`, `p/python` —
and the lane **blocks** (`--error`, no `continue-on-error`). Getting there meant
fixing every real finding and adjudicating every false one.

**The 51 real findings: mutable action tags. All fixed.**

`p/github-actions` found one issue class nothing else in this repo checks —
actions referenced by a **mutable tag** (`actions/checkout@v7`) instead of an
immutable commit SHA. If a tag moves, attacker-controlled code runs with this
repo's `GITHUB_TOKEN`. There are **12** workflow files, **11** of which reference
an action at all (`scan-cron-alarm.yml` has no `uses:`), and every one of the
**56** third-party action references across them is pinned. (The counts move with
every workflow added or removed — they were 13/12/53 when this was written, and
`scripts/check_action_pins_test.go`, not this paragraph, is what actually holds
the invariant.)

```yaml
uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
```

Each SHA is the commit the previously-used tag resolved to at pin time, so the
pin is behaviourally identical to the runs already verified green — a pin should
not smuggle in a version bump. The trailing version comment is also the form
Dependabot reads and updates, and `.github/dependabot.yml` already watches the
`github-actions` ecosystem, so these stay current without hand-editing.

**Two of those pins were not what they looked like, and the failure is silent.**
`git ls-remote` on a repository that publishes *annotated* tags returns the tag
**object's** SHA for `refs/tags/v4`, not the commit it points at — for
`github/codeql-action` that is `4c0873ef…` for `refs/tags/v4` and `db488dde…`
for `refs/tags/v4^{}`. A pin taken from the unpeeled form is a 40-hex string
that looks exactly like a commit pin, satisfies every "is it a SHA" check, and
resolves to a **mutable major tag** — the precise defect the pinning exercise
existed to remove. Two distinct pins were in that state, across 7 usages; both
are now the peeled commit with an exact `# vX.Y.Z` comment, and
`scripts/check_action_pins_test.go` asserts the shape so the next pin cannot be
taken from the wrong ref.

Two `uses:` lines are deliberately left on `@main`: both are inside **comments**
in `build-sandbox-image.yml` / `publish-sandbox-image.yml`, documenting how a
downstream bundle repo calls fleet's reusable workflows. `@main` is the right
guidance for a consumer tracking fleet, and Semgrep does not flag them (a YAML
comment is not a `uses:` key).

**The 6 false positives: suppressed at the line, with reasons.**

Worth reading, because three were **already formally triaged and suppressed for
`gosec`** — which runs inside `golangci-lint` and already blocks — and one is
actively wrong:

| finding | why it is wrong |
| --- | --- |
| `open-redirect` — `cmd/fleet/tls.go` | Standard HTTP→HTTPS upgrade to the **same** host. Already `//nolint:gosec G710`. |
| `math-random-used` — `internal/runner/runner.go` | `math/rand/v2`, used once, for ±10% jitter on a retry interval. |
| `cookie-missing-secure` — `internal/sched/handlers/elcano.go` | A **deletion** cookie (`Value=""`, `MaxAge=-1`), no secret; `Secure` is conditional so logout works over plain-HTTP dev. Already `//nolint:gosec G124`. |
| `unsafe-deserialization-interface` — `internal/mcp/httptool.go` | `json.Unmarshal` into `interface{}` is **required** — the value feeds a jq program over arbitrary JSON. A concrete struct cannot express "whatever shape the response had". |
| `x-frame-options-misconfiguration` — `web/src/proxy.ts` | The header value is the literal string `"DENY"`. No user input reaches it. |
| `insecure-file-permissions` — `internal/sandbox/fileops.py` | Advises `0o644` — **world-readable** — for a sandbox directory. Following it would be a security **regression**; `0750` is the file-tool contract. |

Each carries a line-level `nosemgrep: <rule-id>` naming the specific rule and the
reason. Scoped to the rule, so a *different* rule firing on the same line still
reports.

**Every suppression was mutation-tested.** Removing it makes the finding
reappear; with it, the finding is gone. That matters because "0 findings" has two
explanations — the waivers work, or the rules silently stopped matching — and
only one of them is safety. Checked across all three comment syntaxes (Go `//`,
Python `#`, TypeScript `//`), including the one waiver that had to become a
*trailing* comment because a standalone comment inside a Go import block breaks
`goimports`.

### npm audit owns dependency CVEs for the two npm trees (new, blocking)

`govulncheck` is Go-only and `grype` scans the sandbox *image*, so the web
tier's dependency tree — and `scripts/rampart-service`'s — had no CVE gate at
all. `npm audit --audit-level=low` now runs in the `web` job of both CI lanes,
lockfile-only (no install needed), before the expensive `npm ci`, and fails on
**any** severity. Like govulncheck, its verdict is a function of the clock as
well as the commit: a new advisory can redden an unchanged tree, and that is
the point.

Turning it on surfaced a real backlog immediately:

- `web/` was already clean — 0 vulnerabilities — thanks to the steady stream of
  merged Dependabot PRs.
- `scripts/rampart-service` **had no `package-lock.json` at all**, which meant
  no reproducible installs and nothing for an auditor to read. Generating one
  exposed **5 high-severity vulnerabilities** the missing lockfile had been
  hiding: `sharp <0.35.0` (libvips CVE-2026-33327/-33328/-35590/-35591) and,
  one layer down, `adm-zip <0.6.0` (GHSA-xcpc-8h2w-3j85, crafted-ZIP 4 GB
  allocation) via `onnxruntime-node`.

No upstream release fixes either — the latest `@huggingface/transformers`
still pins `sharp ^0.34.5`, and npm's own suggested "fix" was a breaking
*downgrade* of transformers — so `package.json` carries two `overrides`
(`sharp ^0.35.3`, `adm-zip ^0.6.0`, each the release immediately after the
vulnerable line). The overridden stack was **installed and load-tested**, not
just resolved: sharp renders a PNG through the new libvips, transformers loads
on it, rampart exports its API, and adm-zip 0.6 round-trips a zip. Audit result
after: 0 vulnerabilities in both trees.

An override is a fork of upstream's intent, correct only while upstream is
broken — so `scripts/check-npm-overrides.sh` runs beside the audit in both
lanes and **fails the build the day upstream's own ranges reach the patched
lines**, with removal instructions. The reminder to drop the override is a red
build with a two-line fix, not stale-pin archaeology later. (Registry flake =
skip with a notice, never a verdict; mutation-tested in both directions. The
step invokes it as `"$GITHUB_WORKSPACE/scripts/check-npm-overrides.sh"` — the
job runs under `working-directory: web`, where a repo-relative path resolves
wrong; exit 127 on the first CI run taught that one.)

## Findings are readable from the job log, on purpose

Both scanners print a per-rule summary into the job log **and** the step summary:

The shape of it — the counts and line numbers below are placeholders, since they
move with every commit; what is fixed is the format:

```
### CodeQL findings — go
BLOCKING — High band (security-severity >= 7.0), not waived (<n>):
  none
ACCEPTED — High band, waived in codeql-accepted-findings.json (<n>):
  [error] sec-sev=9.1  go/request-forgery  internal/tools/web_fetch.go:<line>
  [error] sec-sev=7.5  go/clear-text-logging  cmd/fleet/main.go:<line>
  ...
ADVISORY — below the High band; triage in the Security tab (<n>):
  [error] sec-sev=6.1  go/log-injection  <file>:<line>
  ...

totals: <n> finding(s) — <n> blocking, <n> accepted, <n> advisory
rule metadata resolved: <n>
files in the go database: <n>
```

Three properties of that listing are deliberate. Each line carries the
**`file:line`** of the finding, so an agent reading the log can go straight to
the site. The **ACCEPTED tier is printed by name**, because a waiver that is
invisible in CI output is a waiver nobody re-reads. And the two trailing counts
are the coverage lines: `rule metadata resolved` is what the gate's vacuity check
reads (findings present with zero metadata resolved fails the job), and
`files in the … database` distinguishes "no findings" from "analyzed nothing" —
the green-but-vacuous outcome this whole stack exists to rule out.

For the one measurement that is worth quoting rather than illustrating: the Go
database holds **426** of the tree's 427 non-test `.go` files, the missing one
being `host_disabled.go` — `host.go` and `host_disabled.go` carry mutually
exclusive build tags, and the `fleet_host_executor` tag passed to autobuild
deliberately trades which of the two is analyzed in favour of the real
unsandboxed-execution logic. See [`CODEQL.md`](CODEQL.md).

This exists because a CodeQL run reports **nothing** about what it found to its
own log — it writes SARIF, uploads it, exits 0, findings or not. Verified by
grepping a full run's log archive: there is no alert or result count anywhere.
That made a run's real outcome invisible to `gh run view` and to any agent
holding the log but not the code-scanning API.

Semgrep additionally uploads its raw JSON as an artifact (`semgrep-findings`,
14-day retention), so a fixing agent can consume structured findings without
re-running the scan. The repo is public and these results are not sensitive;
withholding them buys nothing.

The parse/scan-error line in that summary is at **zero**, and keeping it there
matters: a partial parse silently drops rules from a file. The three errors it
started with were all fixed for real — `${{ steps.build.outcome }}` interpolated
into a `run:` script in `build-sandbox-image.yml` (moved to `env:`, which is
also the injection-safe form), a `${tag:-(…)}` expansion default whose bare
paren choked the bash sub-parser (hoisted to a plain assignment), and an inline
`import("@playwright/test")` type in `fixtures.ts` (a named `import type`,
validated by `tsc`).

## What gates — the wiring, and where enforcement actually lands

Every lane in the table reaches the branch's aggregate gate:

- `ci-gate` (the single required status check on `main`) `needs` **every other
  job in `ci.yml`** — the docs-only classifier, gitleaks, the actionlint/shellcheck
  workflow+shell lint, the migration DDL lint, the Helm chart lint, Go, ruff,
  CodeQL, Semgrep, web, both Playwright lanes and Grype.
- `Dev gate` `needs` the same set that exists on `dev` — but nothing in the `dev`
  ruleset requires `Dev gate` to be green, so on that branch it is a red check
  rather than a closed gate. That gap is the first item under "Known gaps" and it
  is the single most important qualifier on this whole document.

**That "every other job" is a test, not a habit — and it is the strongest
anti-rot control here, so it should not stay invisible the way it did until
now.** `scripts/check_gate_needs_test.go` parses both workflow files and fails
`make test` if any job is missing from its gate's `needs`. Adding a job and
forgetting to extend `needs` is otherwise a silent one-line regression that
produces a red-but-not-required lane — exactly how the CodeQL Go extraction
break sat unnoticed for weeks. Two sibling tests hold the neighbouring
invariants: `check_action_pins_test.go` (every `uses:` is a 40-hex commit SHA
with an exact version comment) and `check_permissions_test.go` (every workflow
declares a top-level `permissions:` block, so none silently inherits the
repository default).

Two lanes in `ci-gate`'s list are not in the table above because they are not
scanners: **`helm`** lints and renders the Helm chart so a values/template drift
fails here rather than at an operator's install, and **`migrations`** rejects
dangerous DDL in new or changed migration files.

One qualifier on "every lane reaches the gate": on a **docs-only** change the
`changes` job skips the heavy lanes, and `ci-gate` passes over those skips. That
is deliberate and narrowly bounded — the classifier is a prose allow-list, and
`ci-gate` refuses a skip whenever the classifier did *not* say docs-only, so a
skip from any other cause fails the gate.

The scanners get there because `codeql.yml` and `semgrep.yml` are **reusable
workflows** (`on: workflow_call`): `ci.yml` and `dev-ci.yml` each call them as a
job, and a job that calls a reusable workflow sits in a gate's `needs` like any
other job. A scanner finding therefore blocks a merge through the existing
required check — **no branch-protection change, no new required check.**

Worth recording as a correction: an earlier revision of this document claimed
gating the scanners required a repo-settings click, reasoning from "`needs`
cannot cross workflow files". True but incomplete — a `workflow_call` brings the
called jobs *into* the caller's file, which is the standard mechanism and what
ships now. The scanners' own `push`/`pull_request` triggers were removed so
nothing runs twice; each keeps its weekly `schedule` (new queries/rules against
unchanged code) and a `workflow_dispatch`.

**Both scanners fail their job on a finding, but not on the same threshold, and
the difference is deliberate.**

- **Semgrep: any unsuppressed finding.** `--error`, no `continue-on-error`. That
  is defensible because the tree is at zero unsuppressed findings across all four
  packs, with the 6 false positives waived at the line and mutation-tested.
- **CodeQL: an unwaived finding in the High band** (`security-severity >= 7.0`,
  or level `error`/`warning` for a rule that publishes no security-severity),
  with the accepted-findings register applied. Below the band is advisory. It was
  "any finding" for exactly one merge, and [ADR-0048](adr/0048-codeql-severity-gating.md)
  records why that could not hold: nearly every CodeQL security query is
  `@problem.severity error` — `go/log-injection` is `error` at security-severity
  6.1 — so banding on level would block on all 23 log-injection findings, and the
  zero the any-finding gate was armed on came from a diff-informed PR run.

What both thresholds buy is the same thing: a green check that means something
about the *code*, not just that the scanner ran. The analyze step alone exits 0
whether it found nothing or a hundred alerts, which is how the Go toolchain break
survived weeks behind a red-but-not-required check.

What neither buys is enforcement on a branch whose ruleset requires no checks.
Wiring and enforcement are two different levers, and only one of them lives in
this repo.

(Code scanning merge protection — the ruleset's alert-severity rule — remains
available on top as a belt-and-braces option, but nothing depends on it now.)

## Known gaps, deliberately not closed here

Stated rather than left for rediscovery:

- **Nothing in `dev-ci.yml` is a required check on `dev`, so every job in it —
  CodeQL and Semgrep included — is red-but-not-required there.** This is the
  largest gap on the page and it cannot be closed from a pull request, so it is
  written down rather than implied away.

  The `dev` ruleset's only rules are `deletion` and `non_fast_forward`. There is
  no `pull_request` rule and no `required_status_checks` block, so there is no
  status check for GitHub to hold a merge on. `main` is the branch that does
  require one (`CI gate`). Every sentence in this document about a scanner
  "blocking" describes wiring that is genuinely in place — the `workflow_call`
  jobs really do sit in `Dev gate`'s `needs` — and on `dev` that wiring produces
  a red X beside a mergeable PR.

  Two things compound it, and together they are the actual risk:

  1. `.github/dependabot.yml` points the `github-actions` ecosystem at `dev` on a
     **daily** interval with **no `cooldown`** — Dependabot supports `cooldown`
     for `gomod` and `npm` only, so the one ecosystem whose "dependency" is the
     CI definition itself is also the one that cannot be made to wait.
  2. A `github-actions` bump **is a rewrite of `.github/workflows/*`**: it
     changes what CI executes.

  So the shape to avoid is: a same-day patch bump to a third-party action landing
  unattended on a branch with no required checks, rewriting the workflows that are
  supposed to check it.

  **What removes it is that this repository no longer merges anything
  automatically.** `auto-merge-dependabot.yml` was deleted. Its header had argued
  the case against itself — it explained that `gh pr merge --auto` holds a merge
  only on *required* checks, named `dev` as a branch with none, and then listed
  `dev` in its own `branches:` filter, so the mitigation the previous revision of
  this document credited was in fact the delivery mechanism. Every dependency
  bump, every ecosystem, every bump level now waits for a human.

  That closes the compounding risk but **not** the underlying gap: a hand-merged
  PR into `dev` still merges over a red `Dev gate`, because nothing requires it.

  **The remaining fix is a repo-settings action and belongs to the owner:** add
  `Dev gate` to the `dev` ruleset's required status checks. Nothing in a workflow
  file can make itself required, so no PR can close this item.

- **`_test.go` files are outside CodeQL's database** (625 files in this tree —
  the count moves with the suite) — `autobuild` builds packages, not tests.
  Unchanged from default setup, and stated because "CodeQL covers the Go code"
  would otherwise overclaim.
- **The accepted-findings register keys on `(rule, file)`, not
  `(rule, file, line)`.** Deliberate: line numbers churn on every edit, and a
  register that fails on unrelated refactors is a register people delete. The
  cost is that a *second*, genuinely bad instance of an already-waived rule in an
  already-waived file would not block. That is the sharpest edge in the CodeQL
  gate, and it is why each reason string names the specific call sites and the
  guard that makes them safe. See [ADR-0048](adr/0048-codeql-severity-gating.md).
- **A note-level regression no longer fails the build.** A 24th
  `go/log-injection` sink on genuinely untrusted input would appear in the
  advisory tier and the Security tab, not in a red check. The class is not
  unguarded — `gosec`'s G706 covers it inside `golangci-lint`, which *does* block
  through `ci-gate`, and carries a reviewed `//nolint:gosec` annotation at each
  of the ~80 sites where it was adjudicated — but the CodeQL lane is not what
  would stop it.
- **Semgrep's rule packs are registry-fetched and cannot be pinned by
  vendoring** — investigated and rejected on license grounds, not neglect. The
  Semgrep Rules License v1.0 grants use for "your own internal business
  purposes" and states: *"This license does not allow you to distribute the
  rules."* Committing them to this public MIT repo would be redistribution.
  The binary version is pinned; the rules are not, so a registry-side rule
  addition can turn CI red with no commit to blame — named here so a mystery
  red Semgrep run has a first suspect.
- **A red scheduled scan now files an issue** (all four scheduled lanes) — a
  cron failure has no PR to surface it, which is the rot pattern that let the
  CodeQL toolchain break sit red for weeks. Deduped by title; re-failures
  comment on the same issue. Mechanism differs by necessity: govulncheck and
  grype carry an in-job step, while CodeQL and Semgrep are watched by
  `scan-cron-alarm.yml` (a `workflow_run` watcher, which also covers the
  `E2E canary (real model)` lane — three workflows, not two) — because a CALLED
  workflow may not request permissions its caller did not grant, and the check
  fires at plan time before any `if:` can skip the job. In `govulncheck` and
  `grype` the alarm is a separate JOB rather than a step, holding `issues: write`
  on its own so the scan itself does not run beside that scope. Learned by breaking it: an
  `issues: write` alarm job inside the called workflows startup-failed the
  entire calling Dev CI run.
