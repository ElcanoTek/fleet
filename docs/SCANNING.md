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
| `grype` | sandbox image CVEs (fixable CRITICAL) | ~1m | **blocks** (`ci-gate`) | job log + Security tab |
| `gitleaks` | secrets, every branch | ~10s | **blocks** (`ci-gate`) | job log |
| CodeQL | **interprocedural taint / security** | ~2m | **fails on findings** (check red; see below) | job log + Security tab |
| **Semgrep** | **Go/JS/Python SAST + Actions supply chain** | ~40s | **fails on findings** | job log + artifact |

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

The rule selection is deliberately narrow, and `ruff.toml` records the numbers:
default rules find **3** findings on this tree; a broad selection finds **333**,
of which 176 are `%`-format style, 43 magic values and 35 line length. Gating on
that would mean a whole-tree reformat for no correctness gain.

Three real findings were fixed to make the gate clean on day one, so a new
violation is a regression rather than noise in a backlog:

- `internal/mcp/testdata/dummy_server.py` — unused `import os`.
- `bento_doc.py` — **a byte-identical duplicate `has_guard` definition.** Two
  copies, one call site; the second silently shadowed the first. Dead code, and
  the only finding here that was arguably a latent bug.
- `bento_pdf.py` — a lambda assigned to a name (`E731`), rewritten as a `def`.

`ruff format` is reported but **not** gated: the tree has never been
ruff-formatted, so failing on it would block every PR on a reformat nobody
scheduled. The advisory output keeps the size of that decision visible.

### CodeQL owns interprocedural taint (narrowed, fails on findings)

CodeQL is the only tool in this stack that does cross-function dataflow, and that
is exactly the shape of fleet's headline invariants: *a credential must not reach
a log sink, the model context, or the sandbox.* `go/clear-text-logging` is
literally that query. Nothing else here can express it.

So CodeQL keeps its security queries and gives up everything else — the quality
suite duplicated `golangci-lint`/`oxlint` for Go and JS, and ruff is a better fit
for Python. Full reasoning and measurements in [`CODEQL.md`](CODEQL.md).

Its security suite currently reports **zero findings** on this tree, which is
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

## What gates, and what a required check actually means

The lint/test/build lanes reach `main`'s single required status check through
**`ci-gate`**. ruff is inside it. The two scanners are not — they cannot be, since
a job's `needs` cannot reach across workflow files — so each carries its own
aggregate gate job (`CodeQL gate`) or fails directly (Semgrep).

**Both scanners now fail their job on any finding.** That is what makes them
gates rather than reports, and it is only defensible because the tree is at zero
unsuppressed findings in both — verified before switching either on. A gate
turned on over an existing backlog is a gate people route around.

**One half is still yours to close:** a failing check only *blocks a merge* if it
is a required status check. Add **`CodeQL gate`** and **`Semgrep scan`** to the
"Main" ruleset to finish it. A workflow file cannot make itself required.

The distinction that matters for anyone tightening this later:

| block a merge when… | mechanism |
| --- | --- |
| an analysis **failed or did not run** | required status check (`CodeQL gate`) |
| a scanner **found alerts** at/above a severity | **code scanning merge protection** (ruleset → Code scanning rule) |

**A CodeQL job with a hundred open alerts still exits 0 and reports green.** Job
success only says extraction and evaluation worked. That is both why the Go
toolchain break survived weeks behind a red-but-not-required check, and why a
green check is not evidence of a clean tree. See
[`CODEQL.md`](CODEQL.md#two-different-things-can-gate-and-they-are-not-the-same-lever).

## Known gaps, deliberately not closed here

Stated rather than left for rediscovery:

- **No CVE scanning of the web tier's npm tree.** `govulncheck` is Go-only;
  `grype` scans the sandbox *image*. A Next.js app with ~437 TS/JS files has no
  dependency CVE gate. Dependabot opens npm PRs, but Dependabot alerts do not
  block anything. This is the largest remaining hole in the stack — arguably
  larger than anything CodeQL gating would fix.
- **`_test.go` files are outside CodeQL's database** (621 files) — `autobuild`
  builds packages, not tests. Unchanged from default setup.
- **`ruff format` is not enforced.** The tree has never been ruff-formatted:
  9 of 13 files differ, a **3725-line** diff. That is cosmetics, not a finding,
  and landing it inside a security change would bury the security change. One
  command (`ruff format .`) plus flipping the advisory step to a gate, whenever
  someone wants it.
- **ruff's rule set is narrow**, so some real bug classes go unreported.
  Measured: `--select B,SIM,S` adds **21** findings, of which the genuinely
  interesting ones are 2 × `B905` (`zip()` without `strict=` — silent
  truncation) and 1 × `SIM115` (file opened without a context manager). The
  other 18 are `try`/`except`/`pass` in deliberate best-effort cleanup paths.
  Worth a focused pass; not folded in here.
- **Semgrep's own rule packs are network-fetched** from the registry at scan
  time. The semgrep *version* is pinned; the *rules* are not, so a registry
  change can move findings without a diff here. This matters more now the lane
  blocks: a registry-side rule addition can turn CI red with no commit to blame,
  the same class of surprise `govulncheck-scheduled.yml` was created to absorb.
  Vendoring the rules would fix it at the cost of never getting new ones. Left
  as-is deliberately, and named here so a mystery red build has a first suspect.
