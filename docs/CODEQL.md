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
was dropped" below); the *security* side was then widened from the default
suite to `security-extended` once the default measured clean, so the broader
set also started from a zero baseline.

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

The extended suite then verified **clean in CI on all four languages** — Dev CI
run 525 (`32580031374`), the same run that exercises the hardened `actions`
lane — so the fail-on-findings gate holds at the extended tier, not just the
default one.

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

```yaml
push:         branches: [main]
pull_request: branches: [main, dev]
schedule:     - cron: '0 10 * * 1'
```

`push` on `main` mirrors `ci.yml` and produces the alert set of record for the
default branch. `pull_request` on `main` matches what default setup covered.

`dev` on `pull_request` is the **one place this covers more than default setup
did**, and the expansion is deliberate. Every change lands on `dev` first; `main`
only ever receives a promote merge. Scanning `main` alone means a finding
surfaces for the first time on a promote commit — the same complaint
`dev-ci.yml`'s own header already makes about compilation ("a branch whose job is
to integrate should not be where compilation is first attempted"). It is also
what made this change provable before merge: with main-only triggers, the first
real run of a workflow written to fix a silent-failure bug would have happened
*after* it merged.

There is no `push` trigger on `dev`: a push to `dev` is the merge of a PR that
was just scanned, so it would re-analyze identical content.

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

- **No push-on-`main` or scheduled run has executed.** Both triggers are
  unexercised until this merges and is promoted. They are ordinary trigger
  syntax, and the `pull_request` path shares every step with them, but the cron
  expression itself has not fired. It is a weekly cron, so its first real proof
  is up to a week after promotion.
- **`_test.go` files are not analyzed.** 621 test files are outside the
  database, because `autobuild` builds packages, not tests. Default setup did
  not analyze them either, so this is not a regression — it is an unchanged
  limit, stated because "CodeQL covers the Go code" would otherwise overclaim.
  Bringing tests in would need `build-mode: manual`.
- **The lines-of-code metric value was not read.** `Summary/LinesOfCode.ql`
  evaluates, but CodeQL does not print the number to the job log; it lands in a
  `.bqrs`. File and package counts are what was actually observed, so they are
  what is reported here. No line count is claimed.
- **Alert counts on `main` are not claimed.** The zero-security-findings result
  above was measured on a PR run of this branch. PR-run file-coverage detail is
  suppressed by CodeQL ("To speed up pull request analysis, file coverage
  information is only enabled when analyzing the default branch and protected
  branches"), so the default-branch alert set is not established until this
  merges and a promote lands on `main`.
- **`build-mode: manual` was not built.** `autobuild` works, so the more
  complex option was not needed. If `autobuild` regresses, manual mode plus the
  repo's own `go build ./...` is the fallback — and it is also the route to
  analyzing test files.
- **The three existing `upload-sarif` calls were not touched** (`ci.yml`'s Grype
  step, `govulncheck-scheduled.yml`, `grype-scheduled.yml`). They upload their
  own SARIF independently of CodeQL configuration; breaking them would silently
  drop CVE findings from the Security tab.

## Two different things can gate, and they are not the same lever

This distinction is the one most worth internalizing, because a status check on
the CodeQL job does **not** gate on findings:

| you want to block a merge when… | the mechanism | where it lives |
| --- | --- | --- |
| the analysis **failed or did not run** | a required status check on `CodeQL gate` | branch protection / ruleset |
| CodeQL **found alerts** at/above a severity | **code scanning merge protection** | ruleset → "Code scanning" rule |

The second is the one people mean by "gate on CodeQL", and the first does not
give it to you. **A CodeQL job with a hundred open alerts still exits 0 and
reports green** — the job's success only says extraction and query evaluation
worked. That is exactly why the toolchain break was able to hide for weeks behind
a red-but-not-required check, and equally why a green check is not evidence of a
clean codebase.

fleet is a **public** repository, so code scanning merge protection is available
at no cost (on private repos it requires GitHub Advanced Security). To turn it
on: Settings → Rules → the "Main" ruleset → add the **Code scanning** rule →
add tool **CodeQL** → set the alert thresholds. Two independent knobs there:
*Security alerts* (the CWE/security queries — the only ones this workflow runs)
and *Alerts* (everything else, which would be where a code-quality suite landed
if one were enabled; it is not). Since the security suite currently reports zero
findings on this tree, a **High or higher** security threshold can go on without
inheriting a backlog.

## Merge gating today — unchanged, with the lever put within reach

**A finding now turns the check red.** A `Fail on findings` step fails the job on
any finding, at a threshold of *any* — safe to set because the security suite
currently reports zero on this tree, so there is no backlog to grandfather.
Without that step the analyze step exits 0 whether it found nothing or a hundred
alerts, so a red check could only ever mean "the scanner broke" — which is
exactly how the toolchain break hid for weeks.

**A red check now blocks the merge too**, and through the *existing* required
check rather than a new one: `codeql.yml` is a reusable workflow
(`on: workflow_call`) that `ci.yml` and `dev-ci.yml` call as a job, and that
calling job sits in `ci-gate`'s / `Dev gate`'s `needs`. A correction worth
keeping: an earlier revision claimed this half needed a repo-settings click,
reasoning from "`needs` cannot cross workflow files" — true, but a
`workflow_call` brings the jobs into the caller's file, which is the standard
mechanism and what ships.

It *cannot* be folded into `ci-gate`: a job's `needs` cannot reach across
workflow files. So `codeql.yml` carries its own aggregate **`CodeQL gate`** job,
mirroring `ci.yml`'s `CI gate` and `dev-ci.yml`'s `Dev gate`. That job is the one
deliberate piece of forward work here, and it is worth being clear that it
changes nothing on its own:

- It does **not** make CodeQL required. Requiring a check is a repo-settings
  action, deliberately not expressible from a workflow file.
- What it buys is that **flipping the switch later is one check, not four.**
  Naming `Analyze (go)`, `Analyze (python)`, `Analyze (javascript-typescript)`
  and `Analyze (actions)` individually in branch protection would mean
  re-pointing branch protection by hand every time the matrix gains or loses a
  language — and the failure mode of getting that wrong is the dangerous
  direction: a required check that never reports again blocks every PR, or a
  removed one silently stops gating. One aggregate check has neither problem.

No ruleset action is required for any of this: the gate wiring above is entirely
in the workflow files. (`CodeQL gate` still exists as the aggregate job — the
weekly scheduled run's single verdict — and could additionally be named in the
ruleset as belt-and-braces, but nothing depends on that.)

**On sequencing:** Not out of
caution for its own sake — because of this specific incident. The analysis spent
weeks red for a toolchain reason unrelated to any diff, and a required check in
that state blocks *every* merge, including the promote PR that would carry the
fix. Requiring it also means a `dev`-PR CodeQL failure blocks `dev`, a heavier
posture than that lane's stated "does it compile, lint, and pass tests" job. The
sequence with the least chance of self-inflicted deadlock is: merge this, watch a
few promotions go green, then add `CodeQL gate` to the ruleset.

What makes that sequencing *safer than it was*: with code quality dropped, the
security suite is all that runs, and it currently reports **zero findings** on
this tree (see "Why code quality was dropped"). So there is no pre-existing
backlog for a required gate to trip over — which is the usual reason turning one
on hurts. The remaining risk is the one this whole document is about: a toolchain
or extractor regression going red for reasons unrelated to any diff.

No repo-settings or API change to code-scanning configuration was attempted as
part of this change.

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

```
### CodeQL findings — go
2  [error]    go/clear-text-logging
1  [warning]  go/incomplete-hostname-regexp
1  [note]     go/redundant-assignment
--
total findings: 4
```

It is reporting only and never fails the job; blocking on findings is merge
protection's job, above. When no SARIF was written it says so explicitly rather
than printing "No findings." — reporting a clean result you did not observe is
the error this repo keeps having to write down.

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

- **The Security tab's alert list is the DEFAULT BRANCH's.** This workflow's only
  `push` trigger is `main`, so that list refreshes when a promote merge lands on
  `main` — not when a PR is scanned.
- **PR runs report on the PR**, not into the default-branch alert list, and
  CodeQL additionally suppresses file-coverage detail there: *"To speed up pull
  request analysis, file coverage information is only enabled when analyzing the
  default branch and protected branches."*

So after this merges to `dev`, expect findings on subsequent PRs; expect the
Security tab's `main` list to repopulate at the next dev→main promotion.
