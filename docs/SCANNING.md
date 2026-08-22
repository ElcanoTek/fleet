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
| CodeQL | **interprocedural taint / security** | ~2m | advisory | job log + Security tab |
| **Semgrep** | **GitHub Actions supply chain** | ~20s | advisory | job log + artifact |

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

### CodeQL owns interprocedural taint (narrowed, advisory)

CodeQL is the only tool in this stack that does cross-function dataflow, and that
is exactly the shape of fleet's headline invariants: *a credential must not reach
a log sink, the model context, or the sandbox.* `go/clear-text-logging` is
literally that query. Nothing else here can express it.

So CodeQL keeps its security queries and gives up everything else — the quality
suite duplicated `golangci-lint`/`oxlint` for Go and JS, and ruff is a better fit
for Python. Full reasoning and measurements in [`CODEQL.md`](CODEQL.md).

Its security suite currently reports **zero findings** on this tree.

### Semgrep owns GitHub Actions supply chain (new, advisory)

Semgrep is the opposite trade from CodeQL: seconds instead of minutes, no
database build, findings straight to stdout, rules cheap to write. That makes it
the natural fit for an agent-driven loop — and it is why the obvious move is to
point it at everything.

**That move was measured and rejected.** The broad registry packs (`p/golang`,
`p/javascript`, `p/python`) produced 55 findings on this tree, and **all 6
non-Actions findings were false positives:**

| finding | why it is wrong |
| --- | --- |
| `open-redirect` — `cmd/fleet/tls.go:109` | Standard HTTP→HTTPS upgrade to the **same** host. Already carries `//nolint:gosec G710` saying so. |
| `math-random-used` — `internal/runner/runner.go:28` | `math/rand/v2`, used once, for ±10% jitter on a retry interval. |
| `cookie-missing-secure` — `internal/sched/handlers/elcano.go:155` | A **deletion** cookie (`Value=""`, `MaxAge=-1`), no secret; `Secure` is conditional so logout works over plain-HTTP dev. Already `//nolint:gosec G124`. |
| `unsafe-deserialization-interface` — `internal/mcp/httptool.go:254` | `json.Unmarshal` into `interface{}` is **required** — the value feeds a jq program over arbitrary JSON. A concrete struct is not expressible. |
| `x-frame-options-misconfiguration` — `web/src/proxy.ts:99` | The header value is the literal string `"DENY"`. No user input reaches it. |
| `insecure-file-permissions` — `internal/sandbox/fileops.py:77` | Advises `0o644` for a **sandbox directory**, i.e. world-readable. Taking that advice would be a security **regression**; `0750` is the file-tool contract. |

Three of those six were **already formally triaged and suppressed for `gosec`**,
which runs inside `golangci-lint` and already blocks. A scanner that re-reports
adjudicated findings is how you teach a team to ignore it, so those packs are not
enabled. Run them locally for a one-off audit if you want them.

What Semgrep *does* own is the pack that earned it. `p/github-actions` found
**51 instances of one real issue nothing else in this repo checks**: actions
pinned to a **mutable tag** (`actions/checkout@v7`) rather than an immutable
commit SHA. A moved tag runs attacker-controlled code with this repo's token.

```
51  [WARNING]  github-actions-mutable-action-tag
```

Spread across every workflow (18 in `ci.yml`, 7 in `dev-ci.yml`, …).

**It is advisory, and the reason is honesty about scheduling, not doubt about the
findings.** All 51 are real. `--error` here would mean a red gate until every
action in every workflow is repinned to a SHA — a worthwhile change, and its own
PR. Failing CI for a backlog nobody has scheduled just teaches people to ignore
the lane. Flip `continue-on-error` off in the same PR that does the repinning.

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

Everything in the "blocks" column above is reached through **`ci-gate`**, the
single required status check on `main`. The two scanners are outside it.

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
- **Actions are pinned to mutable tags.** 51 instances, per above. Semgrep now
  reports it; nothing yet fixes it.
- **`_test.go` files are outside CodeQL's database** (621 files) — `autobuild`
  builds packages, not tests. Unchanged from default setup.
- **`ruff format` is not enforced**, per above.
- **Semgrep's own rule packs are network-fetched** from the registry at scan
  time. The semgrep *version* is pinned; the *rules* are not, so a registry
  change can move findings without a diff here. Acceptable for an advisory lane;
  it would need a vendored ruleset before this could block.
