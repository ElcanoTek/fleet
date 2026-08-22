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
| `govulncheck` | Go dependency CVEs (called symbols) | ~30s | **blocks** (`ci-gate`) | job log + Security tab |
| `grype` | sandbox image CVEs (fixable **CRITICAL + HIGH**) | ~1m | **blocks** (`ci-gate`) | job log + Security tab |
| `gitleaks` | secrets, every branch | ~10s | **blocks** (`ci-gate`) | job log |
| **`npm audit`** | npm dependency CVEs (web + rampart-service) | ~5s | **blocks** (`ci-gate`) | job log |
| CodeQL | **interprocedural taint / `security-extended`** | ~2m | **blocks** (`ci-gate`/`Dev gate` via workflow_call) | job log + Security tab |
| **Semgrep** | **Go/JS/Python SAST + Actions supply chain** | ~40s | **blocks** (`ci-gate`/`Dev gate` via workflow_call) | job log + artifact |

Two things were added here (**ruff**, **Semgrep**) and one was narrowed
(**CodeQL**, to security queries only).

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

`ruff format --check` is **also gated** (CI and `make lint`): the whole tree
was ruff-formatted in one dedicated commit (9 files, ~3.7k lines, validated
against the full Go suite — the bento/fileops golden tests exercise these
scripts), so the gate started clean and a failure means one new file.

### CodeQL owns interprocedural taint (narrowed, fails on findings)

CodeQL is the only tool in this stack that does cross-function dataflow, and that
is exactly the shape of fleet's headline invariants: *a credential must not reach
a log sink, the model context, or the sandbox.* `go/clear-text-logging` is
literally that query. Nothing else here can express it.

So CodeQL keeps its security queries and gives up everything else — the quality
suite duplicated `golangci-lint`/`oxlint` for Go and JS, and ruff is a better fit
for Python. Full reasoning and measurements in [`CODEQL.md`](CODEQL.md).

It runs the **`security-extended`** suite — the broader security set, adopted
after the default suite measured clean — and currently reports **zero
findings** on this tree, which is
what makes it safe to gate: a `Fail on findings` step now fails the job on any
finding, so a red `Analyze (…)` check means the *code* has a problem rather than
just "the scanner broke". That distinction is the whole reason the Go toolchain
break sat unnoticed for weeks.

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
repo's `GITHUB_TOKEN`. Every one of the **53** action references across all 12
workflows is now pinned:

```yaml
uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
```

Each SHA is the commit the previously-used tag resolved to at pin time, so the
pin is behaviourally identical to the runs already verified green — a pin should
not smuggle in a version bump. The trailing version comment is also the form
Dependabot reads and updates, and `.github/dependabot.yml` already watches the
`github-actions` ecosystem, so these stay current without hand-editing.

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
skip with a notice, never a verdict; mutation-tested in both directions.)

## Findings are readable from the job log, on purpose

Both scanners print a per-rule summary into the job log **and** the step summary:

```
### CodeQL findings — go
2  [note]  go/useless-assignment-to-field
--
total findings: 2
```

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

## What gates — everything, through the gates that already exist

Every lane in the table reaches the branch's aggregate gate:

- `ci-gate` (the single required status check on `main`) `needs` the lint, test
  and build jobs — **and the two scanners**.
- `Dev gate` does the same on `dev`.

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

**Both scanners fail their job on any finding.** That is what makes a green
check mean "clean tree" rather than "the scanner ran" — the analyze step alone
exits 0 whether it found nothing or a hundred alerts, which is how the Go
toolchain break survived weeks behind a red-but-not-required check. Failing on
*any* finding is only defensible because the tree is at zero unsuppressed
findings everywhere — verified before the switch was flipped. A gate turned on
over an existing backlog is a gate people route around.

(Code scanning merge protection — the ruleset's alert-severity rule — remains
available on top as a belt-and-braces option, but nothing depends on it now.)

## Known gaps, deliberately not closed here

Stated rather than left for rediscovery:

- **`_test.go` files are outside CodeQL's database** (621 files) — `autobuild`
  builds packages, not tests. Unchanged from default setup.
- **Semgrep's rule packs are registry-fetched and cannot be pinned by
  vendoring** — investigated and rejected on license grounds, not neglect. The
  Semgrep Rules License v1.0 grants use for "your own internal business
  purposes" and states: *"This license does not allow you to distribute the
  rules."* Committing them to this public MIT repo would be redistribution.
  The binary version is pinned; the rules are not, so a registry-side rule
  addition can turn CI red with no commit to blame — named here so a mystery
  red Semgrep run has a first suspect.
- **A red scheduled scan now files an issue** (all four scheduled lanes:
  CodeQL, Semgrep, govulncheck, grype) — a cron failure has no PR to surface
  it, which is the rot pattern that let the CodeQL toolchain break sit red for
  weeks. Deduped by title; re-failures comment on the same issue.
