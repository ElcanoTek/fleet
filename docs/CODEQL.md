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

| language | build mode | quality queries |
| --- | --- | --- |
| `go` | `autobuild` | yes |
| `python` | `none` | yes |
| `javascript-typescript` | `none` | yes |
| `actions` | `none` | no |

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

Fixed by using `queries: code-quality`, exactly as the message directs.

**This is a real deviation from default setup, not a like-for-like restoration.**
Default setup ran code quality as a separate *analysis kind*, producing a second
analysis that feeds GitHub's Code Quality experience. That kind is
GitHub-internal and closed to custom workflows — no advanced-setup workflow can
feed it. What is restored is the code-quality **query suite**, added to the
default security suite over one shared database, with findings arriving as
ordinary code-scanning alerts. The queries all run; the presentation differs.

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
Distinct queries evaluated, run 1 vs run 2 — the delta is the code-quality suite
arriving:

| language | run 1 | run 2 | delta | SARIF |
| --- | --- | --- | --- | --- |
| `go` | 72 | 116 | +44 | `go.sarif` |
| `python` | 90 | 292 | +202 | `python.sarif` |
| `javascript-typescript` | 178 | 374 | +196 | `javascript.sarif` |
| `actions` | 36 | 36 | +0 | `actions.sarif` |

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
- **Alert counts are not claimed.** This change restores *scanning*; what the
  new quality queries find on this codebase is a separate question, and PR-run
  file-coverage information is suppressed by CodeQL anyway ("To speed up pull
  request analysis, file coverage information is only enabled when analyzing the
  default branch and protected branches").
- **`build-mode: manual` was not built.** `autobuild` works, so the more
  complex option was not needed. If `autobuild` regresses, manual mode plus the
  repo's own `go build ./...` is the fallback — and it is also the route to
  analyzing test files.
- **The three existing `upload-sarif` calls were not touched** (`ci.yml`'s Grype
  step, `govulncheck-scheduled.yml`, `grype-scheduled.yml`). They upload their
  own SARIF independently of CodeQL configuration; breaking them would silently
  drop CVE findings from the Security tab.

## Merge gating — unchanged, and a decision left open

These jobs are **not** wired into `ci-gate`, which is the single required status
check on `main` (see [`.github/CODEOWNERS`](../.github/CODEOWNERS)). CodeQL
findings are therefore advisory, exactly as they were under default setup: a red
CodeQL job does not block a merge.

That is stated as the status quo, not a recommendation. Making CodeQL blocking is
a branch-protection change — a repo-settings click, not a workflow edit — and it
has a real cost worth weighing before anyone makes it: the analysis is advisory
today partly *because* it spent weeks red for a toolchain reason unrelated to any
diff, and a required check in that state blocks every merge. Wiring it into
`ci-gate` would also make `dev`-PR CodeQL failures block `dev`, which is a
heavier posture than that lane's stated "does it compile, lint, and pass tests"
job.

No repo-settings or API change to code-scanning configuration was attempted as
part of this change.
