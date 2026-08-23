# CodeQL: advanced setup, and the Go analysis that had stopped working

Design note for the change that replaced GitHub's zero-config CodeQL "default
setup" with an advanced-setup workflow, `.github/workflows/codeql.yml`.

Companion reading: [`NODE-TOOLCHAIN-HANDOFF.md`](NODE-TOOLCHAIN-HANDOFF.md) —
this is the same failure family (a toolchain version that had a second,
unreconciled copy) and the same rule applies: one declaration point per version,
asserted rather than remembered. [`TESTING.md`](TESTING.md) describes the rest of
the CI ladder.

## What was broken

Default setup's Go analysis failed on every main-targeting PR from the Go 1.27
bump (#1240, promoted in #1242) onward. From PR #1245, job 96968001636:

```
Setup go version spec 1.26
Found in cache @ /opt/hostedtoolcache/go/1.26.6/x64
...
Run github/codeql-action/autobuild@v4
  env: GOTOOLCHAIN: local
...
go: go.mod requires go >= 1.27.0 (running go 1.26.6; GOTOOLCHAIN=local)
Failed to run `go mod tidy -e` in .
make: *** [Makefile:72: compile] Error 1
Error running go tooling: exit status 1
Extraction failed for all discovered Go projects.
CodeQL job status was configuration error.
```

Default setup installed the Go its extractor was built with (1.26.6) and ran
under `GOTOOLCHAIN=local`, so against a `go.mod` requiring 1.27 it could neither
use the local toolchain nor download a newer one. It also invokes `make`, which
puts this repo's Makefile on the autobuild path.

The consequence is the part worth stating plainly: **the repo had no CodeQL
coverage of its Go code for that entire stretch.** The failure was loud (red
checks) but not blocking — `ci-gate` is the only required check on main — so it
was annotated in promote commit messages and lived on.

Default setup is zero-config: it exposes no Go version input and no env, so there
was nothing to fix in place. Hence advanced setup.

## A correction to the diagnosis

The working assumption going in was that the `GOTOOLCHAIN=local` pin came from
GitHub's *generated* default-setup workflow, and that a plain advanced workflow
would therefore not have it at all.

**That is wrong, and it was checked rather than assumed.** `GOTOOLCHAIN: local`
is set by `github/codeql-action` itself. It appears in the environment of four
separate steps in our own workflow's Go job — `Set up Go`, `Initialize CodeQL`,
`Autobuild`, and `Perform CodeQL Analysis`:

```
$ grep -rn 'GOTOOLCHAIN' 'Analyze (go)'/
./3_Set up Go.txt line 74
./4_Initialize CodeQL.txt line 15
./5_Autobuild.txt line 10
./6_Perform CodeQL Analysis.txt line 18
```

So the fix is **not** "the pin is gone". The pin is still there. The fix is that
`actions/setup-go` makes the *local* toolchain 1.27.0, which is what `go.mod`
asks for, so `GOTOOLCHAIN=local` is satisfied instead of contradicted. The
autobuilder notices and proceeds:

```
Autobuilder was built with go1.26.5, environment has go1.27.0
```

This matters for the next person: the mechanism is "give `local` something good
enough", not "unset the pin". Neither `env: GOTOOLCHAIN: auto` nor
`build-mode: manual` — the two fallbacks held in reserve — was needed.

## What shipped

`.github/workflows/codeql.yml`, one `analyze` job over a four-entry matrix.

| language | build mode | queries |
| --- | --- | --- |
| `go` | `autobuild` | `security-extended` |
| `python` | `none` | `security-extended` |
| `javascript-typescript` | `none` | `security-extended` |
| `actions` | `none` | `security-extended` |

**Security queries only — at the `security-extended` tier.** The code-quality
suite was enabled, measured, and deliberately removed (see "Why code quality
was dropped" below); the *security* side was then widened from the default suite
to `security-extended`. The widening was done on the belief that the default
suite had "measured clean" and that the broader set therefore also started from a
zero baseline. **That belief was an artifact of measuring on a PR run** — see
"The threshold, and the measurement that was misread" below. The suite choice
still stands; the baseline claim did not.

Adopting the extended suite was a measurement, and it produced exactly **one
finding across all four languages**: `actions/untrusted-checkout/medium` on the
`workflow_call` checkout in `build-sandbox-image.yml`, whose `ref:` is fed by
the `fleet_ref` input. The query is a **name heuristic** — it flags any
checkout whose ref traces to a field matching `.*(head|branch|ref).*` —
reproduced locally with the CodeQL 2.26.3 bundle to confirm the trigger before
touching anything. Two honest responses existed and the in-code dismissal was
not one of them: the `actions` language ships **no `AlertSuppression.ql`**, so
there is no comment-waiver path at all. The finding was fixed for real instead:
a `pin` step now refuses `refs/pull/*` / `pull/*` refs (a fork PR ref would
carry fork-controlled code into a workflow that *executes the checked-out build
script*), and the checkout consumes that step's neutrally-named output. The
same hardening went into `publish-sandbox-image.yml` — the **unflagged twin**
that is strictly more dangerous (it holds `packages: write`) but escaped the
query because its ref plumbing was named differently. A heuristic query's
silence is not evidence of safety; the flagged file just pointed at the class.

The extended suite then reported **no findings in CI on all four languages** —
Dev CI run 525 (`32580031374`), the same run that exercises the hardened
`actions` lane. Read that sentence narrowly: run 525 was a `pull_request` event,
so what it establishes is that the extended suite found nothing **inside that
PR's diff**. The tree-wide numbers came later and were not zero. The full account
is in "The threshold, and the measurement that was misread".

`build-mode: none` is [not supported for
Go](https://docs.github.com/en/code-security/reference/code-scanning/codeql/build-options-for-compiled-languages)
— only `autobuild` or `manual` — so Go's toolchain has to be correct rather than
skipped.

**The Go interpreter is resolved from `go.mod`,** via `actions/setup-go` with
`go-version-file: go.mod`, never a literal. A hardcoded `1.27` here would be the
same bug class #1240 and #1241 already fixed twice for node.

That rule is enforced, and the enforcement was **already directory-wide**:
`scripts/check_versions_test.go`'s `TestWorkflowsDeclareVersionsByFile` walks
every `.github/workflows/*.yml` and fails on any literal `go-version:` /
`node-version:`. So `codeql.yml` came under the assertion the moment it was
added — **no test change was needed.** Verified by breaking it on purpose rather
than by reading the regex:

```
$ sed -i "s|go-version-file: go.mod|go-version: '1.27.0'|" .github/workflows/codeql.yml
$ go test -count=1 -run TestWorkflowsDeclareVersionsByFile ./scripts
--- FAIL: TestWorkflowsDeclareVersionsByFile (0.00s)
    check_versions_test.go:149: codeql.yml pins a literal version ("go-version: '1")
        — use `node-version-file: web/.nvmrc` or `go-version-file: go.mod` so the
        version has one declaration point
```

### Triggers

`codeql.yml` has **no `push` and no `pull_request` trigger of its own.** It is a
reusable workflow:

```yaml
on:
  workflow_call:      # ci.yml (main) and dev-ci.yml (dev) each call it as a job
  workflow_dispatch:  # manual re-run
  schedule:
    - cron: '0 10 * * 1'   # Monday 10:00 UTC
```

Per-change runs therefore arrive through `workflow_call`: `ci.yml` fires on
push/PR against `main`, `dev-ci.yml` on push/PR against `dev`, and each calls
this workflow as a job. Every branch event is covered exactly once, and — because
a called workflow's jobs land in the *caller's* graph — the result feeds
`CI gate` / `Dev gate` directly. Scanning `dev` PRs at all is the one place this
exceeds old default setup, which never ran on them.

**An earlier revision of this section was wrong in a way worth preserving,
because the error is instructive.** It described a `push: [main]` /
`pull_request: [main, dev]` trigger set and justified omitting `push` on `dev`
like this: *"a push to `dev` is the merge of a PR that was just scanned, so it
would re-analyze identical content."*

Both halves are false, and the second is the exact trap that later broke `dev`.
The PR run and the push run **do not analyze identical content**: on a
`pull_request` event the CodeQL action runs **diff-informed** — it builds the
full database and evaluates every query, then reports only results whose location
falls inside the PR's diff. A push run has no diff to scope to and reports the
whole tree. So the two events differ in the most consequential way an analysis
can differ: one certifies a diff, the other certifies a tree. Treating them as
interchangeable is how an any-finding gate got armed on a zero that had never
seen the tree. See [ADR-0048](adr/0048-codeql-severity-gating.md), and "The
threshold, and the measurement that was misread" below.

The current shape has no such hole: `dev-ci.yml` calls this workflow on **pushes
to `dev` as well as PRs into it**, so `dev` gets a full-tree verdict on every
merge, and any direct push that bypassed a PR is covered too.

The weekly cron exists because a CodeQL verdict is a function of the query pack
as well as the commit — new queries ship continuously, and without a schedule the
only way this repo learns that a newly published query flags existing code is
that some unrelated PR turns red. That is the argument
`govulncheck-scheduled.yml` already makes about `vuln.go.dev`. Monday 10:00 UTC
is offset from the 07:00 canary, the 08:00 daily govulncheck and the Monday 09:00
Grype scan, following the same don't-contend-for-runners note those files carry.

`dev-ci.yml`'s header used to list CodeQL among the checks deferred to the
dev→main gate. That is no longer true, so it now says where CodeQL runs instead.

## Two defects in the first cut, both green

Both were found by reading the run log. Both runs of this workflow were **green**
while neither behaviour worked. This is the failure mode the change exists to
fix, so it is worth being concrete: a passing CodeQL job proves nothing about
what was analyzed.

### 1. `analysis-kinds` is not available to us

The first attempt passed `analysis-kinds: code-scanning,code-quality`, on the
reading that one init call could build one database and run both suites. The
action emitted two `##[error]` lines — **and exited 0**:

```
The `analysis-kinds` input is experimental and for GitHub-internal use only.
[...] An analysis kind other than `code-scanning` was specified in a custom
workflow. This is not supported and will become a fatal error in a future
version of the CodeQL Action. If your intention is to use quality queries
outside of Code Quality, use the `queries` input with `code-quality` instead.

[...] Specifying multiple values as input is no longer supported. Continuing
with only `analysis-kinds: code-scanning`.
```

Confirmed in the artifacts rather than trusted from the warning: the Go job
loaded only `codeql/go-queries`, evaluated 72 distinct queries, and uploaded a
single `go.sarif`. The code-quality half of the coverage this change claims to
restore was not running at all.

Fixed at the time by using `queries: code-quality`, exactly as the message
directs. That is the working form, and it is worth recording for anyone who tries
`analysis-kinds` again and sees a green check: the input is accepted, two errors
are logged, and only security runs.

Code quality was then measured and dropped — see the next section. The lasting
point from this defect is the one about evidence: the run was **green** with the
requested analysis silently not happening.

### 2. Go extraction had one hole, and it was the worst file in the tree

The first run extracted 426 files. The tree has 427 non-test `.go` files. The
missing one, by diffing the extractor's own file list against the checkout:

```
$ comm -13 extracted.txt local_nontest.txt
internal/sandbox/host.go
```

`internal/sandbox/host.go` is the **unsandboxed host executor** — bash and python
run directly on the host — fenced behind `//go:build fleet_host_executor` and so
absent from the default build. It is a CODEOWNERS-protected path, and both
`ci.yml` and `dev-ci.yml` deliberately pass that tag to `go vet` and `go test`
("Same tag as ci.yml so host.go [...] is vetted too") precisely so it is not
left unchecked. Leaving it as the single gap in Go coverage is not a defensible
default.

`GOFLAGS: -tags=fleet_host_executor` on the autobuild step fixes it, matching the
lanes that already vet it.

**The honest limit:** the total is still 426, because `host.go` and
`host_disabled.go` carry mutually exclusive build tags (`fleet_host_executor` and
`!fleet_host_executor`), so exactly one is ever in a build. The tag trades which
one CodeQL sees. That is a good trade — `host.go` is 410 lines of real
unsandboxed-execution logic, `host_disabled.go` is a 26-line refusal stub — but
it is a trade, not the elimination of a gap. Analyzing both would need two
databases.

Note also that the extractor still logs `Build flags: ''`. `GOFLAGS` reaches the
`go` tooling through the environment, not through the extractor's own flag
plumbing, so the log line that looks like it should confirm the fix does not.
`host.go` appearing in the extracted list is what confirms it.

## Why code quality was dropped

The quality suite shipped first, ran, and was then removed on the evidence it
produced. Worth writing down, because "more queries" reads as strictly better
until you look at what they found.

**What it found: 32 findings, every one note-level, and zero security findings.**

(The "zero security findings" half of that sentence was measured on `pull_request`
runs and is therefore a statement about those diffs, not about the tree — see
"The threshold, and the measurement that was misread". It does not change the
drop decision, which rests on the 32 quality findings and where they were: those
were *reported* results, and the argument against them is that other blocking
tools already cover the same ground.)

| language | findings | what they were |
| --- | --- | --- |
| `go` | 2 | `go/useless-assignment-to-field` |
| `python` | 28 | `py/empty-except` (12), `py/implicit-string-concatenation-in-list` (9), `py/comparison-of-identical-expressions` (3), unused local/global/import (4) |
| `javascript-typescript` | 2 | `js/trivial-conditional`, `js/useless-assignment-to-local` |
| `actions` | 0 | — |

Three reasons that adds up to "wrong tool", not "clean codebase":

1. **For Go and the web tier it duplicates gates that already block.**
   `golangci-lint` runs `gosec`, `staticcheck`, `revive`, `unparam`, `gocritic`
   and more, and it is inside `ci-gate`; oxlint covers the web tier.
   `go/useless-assignment-to-field` is squarely inside that remit. Paying ~40s of
   CodeQL for a second opinion on it buys nothing.

2. **28 of 32 were Python — a real gap, but ruff is the right instrument.**
   Python genuinely had no linter (see `ruff.toml`), so those findings were the
   suite's only unique contribution. ruff finds the same class of thing in well
   under a second, with autofix, and now blocks. A slow job with no autofix is
   the wrong shape for lint, especially for an agent expected to fix and re-push.

3. **Three of the 32 were false positives on correct code.**
   `py/comparison-of-identical-expressions` flagged `value != value` three times
   in `bento_pdf.py` — the idiomatic NaN test, which is true only for NaN.
   Enabling the equivalent ruff rule (`PLR0124`) was rejected for the same
   reason.

So the security queries stay (they are what nothing else here can do — see the
Semgrep comparison in `.github/workflows/semgrep.yml`), and quality moves to the
linters that were already gating.

**What this costs, stated plainly:** the four Go/JS quality findings above are no
longer reported by anything, because `golangci-lint` and oxlint did not
independently flag them. That is a real, small loss of coverage accepted in
exchange for not running a second slow analyzer over ground three other tools
already cover.

## What was verified

Measured on run 2 (`7731615`, run `32571297663`), by downloading the run's log
archive and reading the extractor's and evaluator's own output — not from the
check mark.

**Go extraction really happened:**

```
Found 2 go.mod files in: go.mod, web/go.mod.
Done running go list deps: resolved 916 packages.
Done extracting .../internal/sandbox/host.go
Success: extraction succeeded for all 2 discovered project(s).
```

426 distinct `.go` files extracted, and the set differs from the tree's 427
non-test files by exactly `host_disabled.go`, per the build-tag trade above.

**Every language produced a database, ran queries, and uploaded results.**
Distinct queries evaluated. The middle column is the security suite alone, which
is what ships; the right column is what adding `queries: code-quality` did, kept
here because it is the measurement the drop decision rests on:

| language | security only (ships) | with code-quality (dropped) | SARIF |
| --- | --- | --- | --- |
| `go` | 72 | 116 (+44) | `go.sarif` |
| `python` | 90 | 292 (+202) | `python.sarif` |
| `javascript-typescript` | 178 | 374 (+196) | `javascript.sarif` |
| `actions` | 36 | 36 (+0) | `actions.sarif` |

`actions` is unchanged **by design** — default setup ran it in the security
analysis only, and that was matched rather than widened. The new Go query
directories are `RedundantCode` and `InconsistentCode`; JavaScript gains
`Quality`; Python gains `Classes`, `Exceptions`, `Functions`, `Imports`,
`Lexical`, `Resources`, `Statements`, `Testing` and `Variables`. All four jobs
logged `Successfully uploaded results`.

**`web/` is in scope and contributes nothing, by design.** The autobuilder
discovers both `go.mod` files and extracts both projects. `web/` reports:

```
Running extractor command '.../go-extractor [./...]' from directory 'web'.
No packages found.
Done running go list deps: resolved 0 packages.
```

That is the correct outcome and matches `web/go.mod`'s own comment — it is a
no-package boundary module that exists to stop root `go ... ./...` traversing Go
source vendored inside `node_modules`. It was left in scope rather than excluded:
extraction of an empty module is free, and excluding it would need a config file
whose only job is to suppress something harmless. **Verified, not assumed** —
this is the specific claim the old autobuild failure ("Extraction failed for all
discovered Go projects") made it reasonable to worry about.

**The local gate**, on this branch: `make build`, `make lint` (0 issues),
`make test` (exit 0), `make lint-migrations` (no changed migrations). `make lint`
needed `golangci-lint` v2.13.1 built with Go 1.27 — the installed 2.5.0 was built
with go1.25.1 and cannot lint the tree.

## What was NOT verified, and what is deliberately out of scope

- **No push-on-`main` or scheduled run had executed when this section was first
  written.** That is no longer true of the push path: `dev-ci.yml` calls this
  workflow on pushes to `dev`, and the first such run (Dev CI run 527) is the
  full-tree measurement described below — it is also what proved the section
  above wrong. The **weekly cron** is still the one trigger whose own first proof
  is up to a week away; the `schedule` path shares every step with the others, so
  what is unexercised is the cron expression, not the analysis.
- **`_test.go` files are not analyzed.** Every `_test.go` file is outside the
  database (625 in this tree; the count moves with the suite), because
  `autobuild` builds packages, not tests. Default setup did not analyze them
  either, so this is not a regression — it is an unchanged limit, stated because
  "CodeQL covers the Go code" would otherwise overclaim. Bringing tests in would
  need `build-mode: manual`.
- **The lines-of-code metric value was not read.** `Summary/LinesOfCode.ql`
  evaluates, but CodeQL does not print the number to the job log; it lands in a
  `.bqrs`. File and package counts are what was actually observed, so they are
  what is reported here. No line count is claimed.
- **Alert counts on `main` are still not claimed, and the reason turned out to
  matter far more than expected.** The no-findings result above was measured on a
  **PR run**, where CodeQL is diff-informed and additionally suppresses
  file-coverage detail ("To speed up pull request analysis, file coverage
  information is only enabled when analyzing the default branch and protected
  branches"). So it never established a tree-wide baseline for `dev`, let alone
  for `main`. The `dev` tree-wide numbers now exist (run 527, below); the
  default-branch alert set is established when a promote lands on `main`.
- **`build-mode: manual` was not built.** `autobuild` works, so the more
  complex option was not needed. If `autobuild` regresses, manual mode plus the
  repo's own `go build ./...` is the fallback — and it is also the route to
  analyzing test files.
- **The three existing `upload-sarif` calls were not touched** (`ci.yml`'s Grype
  step, `govulncheck-scheduled.yml`, `grype-scheduled.yml`). They upload their
  own SARIF independently of CodeQL configuration; breaking them would silently
  drop CVE findings from the Security tab.

## The threshold, and the measurement that was misread

This is the most load-bearing correction in this document, and it generalises
beyond CodeQL. The decision is recorded as
[ADR-0048](adr/0048-codeql-severity-gating.md); what follows is the short form.

The `Fail on findings` step originally failed the job on **any finding at any
severity**, with this justification written into the workflow:

> Threshold is ANY finding, deliberately. The security suite currently reports
> ZERO across go/python/javascript-typescript/actions, so there is no backlog to
> grandfather and no severity line to argue about — a finding here is new.

That zero was real, and it was measured — **on a `pull_request` event** (Dev CI
run 525). On `pull_request` events the CodeQL action runs **diff-informed**: it
builds the full database and evaluates every query, then reports only results
whose location falls inside the PR's diff. Run 525's own log says both halves:

```
Computing PR diff ranges...
Persisted 204 diff range(s) across 43 file(s).
codeql database run-queries ... --extension-packs=codeql-action/pr-diff-range
```
```
To speed up pull request analysis, file coverage information is only enabled
when analyzing the default branch and protected branches.
```

The database held every file and the queries that later fired did run —
`LogInjection.ql`, `TaintedPath.ql`, `RequestForgery.ql` and
`WeakSensitiveDataHashing.ql` are all listed as "Interpreted" in that run. The
SARIF was empty because the results were filtered to the PR's 43 changed files.

So the first full-tree evaluation of `security-extended` against this repository
was the **push** that merged that work: **Dev CI run 527**, which reported **38 Go
and 17 javascript-typescript findings** and turned `Dev gate` red. The gate then
blocked every subsequent push to `dev` — including a push that would have fixed
it — with no PR-shaped way out, because a PR into `dev` is scanned diff-informed
and comes back green while `dev` itself stays red.

**The rule to carry away: a PR-event CodeQL run certifies a diff, not a tree.**
Any claim of the form "the scanners are green, therefore the tree is clean" that
rests on a `pull_request` run is unsound, and that is a permanent property of
diff-informed analysis rather than a bug awaiting a fix. Tree-wide verdicts come
from push and scheduled runs.

### What the threshold is now

A finding **blocks** when it is not waived and either:

- its rule publishes `security-severity >= 7.0` — CodeQL's own High/Critical cut,
  and what GitHub's code-scanning merge protection bands on; or
- its rule publishes **no** security-severity at all, in which case the fallback
  is the SARIF level `error` or `warning`.

Level is deliberately **not** consulted for a rule that does publish a
security-severity. Nearly every CodeQL security query is
`@problem.severity error` — `go/log-injection` is `error` at security-severity
**6.1** — so banding on level would put all 23 log-injection findings in the
blocking tier and reproduce the deadlock. For orientation, the severities that
actually appear on this tree:

| rule | security-severity | tier |
| --- | --- | --- |
| `go/request-forgery` | 9.1 | High band (waived per-file) |
| `go/clear-text-logging` | 7.5 | High band (waived per-file) |
| `go/path-injection` | 7.5 | High band (waived per-file) |
| `go/weak-sensitive-data-hashing` | 7.5 | High band (waived per-file) |
| `js/remote-property-injection` | 7.5 | High band (waived per-file) |
| `js/insecure-temporary-file` | 7.0 | High band (fixed, then waived) |
| `go/log-injection` | 6.1 | advisory |
| `js/client-side-request-forgery` | 5.0 | advisory |

Everything below the band is **advisory**: printed in the job log and the step
summary, uploaded to the Security tab, not blocking.

Note what that table shows: **severity alone does not separate the true positives
from the false ones.** The 9.1 `go/request-forgery` fires on `web_fetch.go`, the
deliberate `@url` fetch tool, which dials through `internal/netguard`'s
resolve-then-dial SSRF guard. The 7.5 `go/weak-sensitive-data-hashing` fires on
SHA-256 used as a lookup index over a 32-byte `crypto/rand` bearer token — the
recommended construction. A pure severity line would block both. That is why the
band comes with a register rather than instead of one.

### The register, and why it is per-file

`.github/codeql-accepted-findings.json` lists accepted `(rule, file)` pairs, each
with a **mandatory written reason** that must say why the finding cannot be
exploited *there* — not that the rule is noisy.

Per-**file** is the whole point of preferring it to a `query-filters` exclude. An
`exclude: {id: go/request-forgery}` switches a security-severity 9.1 query off
for the entire repository; a register entry waives it in
`internal/tools/web_fetch.go` and `internal/mcpoauth/discovery.go` and leaves the
query live everywhere else, including elsewhere in those same packages.

The register is also the **only** waiver route. An in-source `// codeql[rule-id]`
comment is the mechanism CodeQL documents, and it does **not** work with this
pipeline — measured on PR #1249: three forms were tried (the `packs:` input, `packs:` with the additive `+` prefix, and an inline `config:` combining security-extended with codeql/go-queries' `AlertSuppression.ql`) and in every case the uploaded SARIF carried no `suppressions` on the annotated result, the gate kept classifying the waiver from the register,
and the Security-tab alert stayed open. The analyze action's interpret step is
not configurable enough to change that. A deliberately-waived alert is closed in
the Security tab by a one-time human dismissal, which persists across analyses.

Of the 55 findings run 527 surfaced, **four were reachable and were fixed in
code**: an unsanitized `task.Prompt` in the task-create log (its update-path twin
was already wrapped in `logSafe`), the raw pre-validation client attachment path
logged on the two branches where containment had just failed, the client-echoed
attachment `Name` on the `/chat` path, and an Ed25519 private key written to a
predictable world-writable temp path at `0644` in `web/e2e/test-auth-key.ts`. The
other 51 are in the register.

### One classifier, three tiers, and it fails closed

`.github/codeql-gate.jq` does the banding and the waiver lookup, and **both** the
summary step and the gate step run it through `jq -f`. Two copies of a SARIF
filter is two copies that can disagree about what "blocking" means, and the
report disagreeing with the gate is worse than either being wrong alone.

The job log prints three tiers from that single classification — **BLOCKING**,
**ACCEPTED** (by name, because a waiver that is invisible in CI output is a waiver
nobody re-reads) and **ADVISORY**.

It fails the job rather than reporting clean when: the register is missing, the
filter file is missing, the SARIF will not parse, or — the subtle one — findings
exist but **zero rule metadata resolved**. That last is a vacuity check with real
provenance: CodeQL writes query metadata into `tool.extensions[].rules[]`, not
`tool.driver.rules[]`. A first cut of the filter read only the driver, resolved
nothing, scored every finding at security-severity 0 — `go/request-forgery`'s 9.1
included — and reported "0 blocking" over a tree that was not clean. That is the
green-but-vacuous outcome this entire workflow exists to rule out, so it is now
an explicit failure mode rather than a silent one.

Three anti-rot controls sit on top: `scripts/check_codeql_register_test.go` (in
`make test`) requires every entry to name a file that exists, carry a substantive
reason, use a plausible rule id, and be unique — and asserts that `codeql.yml`
still references the register at all; the weekly scheduled scan surfaces entries
that no longer match any finding, so a stale waiver gets removed rather than
quietly widening coverage loss; and widening the register shows up in a PR diff
where the reviewer is expected to check the reason against the code.

## Two different things can gate, and they are not the same lever

This distinction is still worth internalizing, because the two mechanisms answer
different questions:

| you want to block a merge when… | the mechanism | where it lives |
| --- | --- | --- |
| the analysis **failed, did not run, or found something in the blocking band** | the `Fail on findings` step, reaching a required aggregate check | this repo's workflow files |
| CodeQL **found alerts** at/above a severity, judged from the Security tab's alert set | **code scanning merge protection** | ruleset → "Code scanning" rule |

The first row is what ships, and the wording has been corrected: an earlier
revision of this document said **"a CodeQL job with a hundred open alerts still
exits 0 and reports green"**, and that is no longer true. It was true of the
`analyze` step alone, and it is precisely why the `Fail on findings` step exists.
The step reads the run's **own SARIF** and never consults the code-scanning API,
which has one consequence worth stating plainly: **dismissing an alert in the
Security tab does not turn this check green.** The honest routes are a code
change or a register entry with a reason — an in-source `// codeql[rule-id]`
comment is not one of them here (see above).

The second row remains available and nothing depends on it. fleet is a **public**
repository, so code scanning merge protection is free (on private repos it needs
GitHub Advanced Security): Settings → Rules → the "Main" ruleset → add the **Code
scanning** rule → add tool **CodeQL** → set the thresholds. Two independent knobs
there: *Security alerts* (the CWE/security queries — the only ones this workflow
runs) and *Alerts* (everything else, which is where a code-quality suite would
land if one were enabled; it is not). Note that a **High or higher** threshold
there would *not* start from a clean slate: the tree carries High-band findings
that are accepted in the register, and merge protection has no view of that
register. It bands on the Security tab's alert set, so those alerts would need
dismissing individually in the UI.

## Merge gating today

**A finding in the blocking band turns the check red.** The `Fail on findings`
step is what makes that true; the threshold is the High band described above, not
"any finding". Without the step, `analyze` exits 0 whether it found nothing or a
hundred alerts, so a red check could only ever mean "the scanner broke" — which
is exactly how the toolchain break hid for weeks.

**A red check reaches the aggregate gate**, through the *existing* check rather
than a new one: `codeql.yml` is a reusable workflow (`on: workflow_call`) that
`ci.yml` and `dev-ci.yml` call as a job, and that calling job sits in
`ci-gate`'s / `Dev gate`'s `needs`. A correction worth keeping: an earlier
revision claimed this half needed a repo-settings click, reasoning from "`needs`
cannot cross workflow files" — true of a job's `needs`, but a `workflow_call`
brings the called jobs *into* the caller's file, which is the standard mechanism
and what ships. (An even earlier revision of this section then went on to repeat
the original error two paragraphs later. Both are corrected here.)

**Whether a red check blocks a merge is a separate, branch-dependent fact, and on
`dev` the answer is no.** `CI gate` is a required status check on `main`, so
there the routing closes. The `dev` ruleset requires **no status checks at all** —
its only rules are `deletion` and `non_fast_forward` — so `Dev gate` is
red-but-not-required, and a CodeQL failure on `dev` is a red X beside a mergeable
PR. Adding `Dev gate` to the `dev` ruleset is a repo-settings action that no pull
request can perform; it is tracked as an open item in
[`SCANNING.md`](SCANNING.md) ("Known gaps").

`codeql.yml` also carries its own aggregate **`CodeQL gate`** job. In the
`workflow_call` path it is redundant — the caller's `needs: codeql` already rolls
up every matrix leg — so what it is for is the standalone schedule/dispatch runs
(one legible verdict per weekly re-scan instead of four boxes) and as a stable
single check name should anyone want to name one in a ruleset. Naming
`Analyze (go)`, `Analyze (python)`, `Analyze (javascript-typescript)` and
`Analyze (actions)` individually would mean re-pointing branch protection by hand
every time the matrix gains or loses a language, and the failure mode of getting
that wrong runs in the dangerous direction: a required check that never reports
again blocks every PR, and a removed one silently stops gating.

No repo-settings or API change to code-scanning configuration was made as part of
this work.

## Where findings appear — and why the job log now says

A CodeQL run reports **nothing about what it found** to its own log. It writes
SARIF, uploads it, exits 0, and the only lines resembling a result are
`Exporting results to SARIF...` and `Successfully uploaded results` — which say a
file moved, not what was in it. Verified by grepping a full run's log archive for
any alert or result count: there is none.

That makes a run's actual outcome invisible to anyone reading CI output, to
`gh run view`, and to any automation holding the log but not the code-scanning
API. So the analyze step now also writes SARIF locally (`output:`) and a
following step jq-summarizes it into both the job log and the step summary — the
same thing `govulncheck-scheduled.yml` already does with its SARIF:

The counts and line numbers below are placeholders — they move with every commit.
What is fixed is the format:

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

Three properties of that listing are deliberate. Every line carries the
finding's **`file:line`**, so an agent reading the log can go straight to the
site. The **ACCEPTED tier is printed by name**, because a waiver invisible in CI
output is a waiver nobody re-reads. And the two trailing counts are coverage
lines rather than verdicts: `rule metadata resolved` is what the vacuity check
reads, and `files in the … database` is what distinguishes "no findings" from
"analyzed nothing".

The summary step is reporting only — a **separate** `Fail on findings` step does
the blocking, from the same classification (see "The threshold" above), so the
report and the gate cannot disagree. When no SARIF was written the step says so
and **fails**, rather than printing "No findings." — reporting a clean result you
did not observe is the error this repo keeps having to write down.

`security-events: write` plus the analyze step's upload is the code-scanning
ingestion path, so results also land in the repo's **Security → Code scanning**.
From the run log:

```
Adding fingerprints to SARIF file. See ... sarif-support-for-code-scanning ...
##[group]Uploading code scanning results
Uploading results
Successfully uploaded results
Analysis upload status is complete.
```

Two practical consequences worth stating, because they explain an empty-looking
Security tab rather than a broken one:

- **The Security tab's alert list is the DEFAULT BRANCH's.** This workflow has no
  `push` trigger of its own at all; push-event runs reach it through `ci.yml`
  (on `main`) and `dev-ci.yml` (on `dev`). So the default-branch list refreshes
  when a promote merge lands on `main` — not when a PR is scanned, and not when
  `dev` moves.
- **PR runs report on the PR**, not into the default-branch alert list, and
  CodeQL additionally suppresses file-coverage detail there: *"To speed up pull
  request analysis, file coverage information is only enabled when analyzing the
  default branch and protected branches."*

So after this merges to `dev`, expect findings on subsequent PRs; expect the
Security tab's `main` list to repopulate at the next dev→main promotion.
