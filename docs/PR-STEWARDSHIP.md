# PR stewardship — the reference

The **procedure** for driving a fleet pull request to green after it is opened
lives in the skill at
[`.agents/skills/steward/SKILL.md`](../.agents/skills/steward/SKILL.md): read
that first, and read it before acting on any PR event. This page is the
reference behind it — what each CI lane actually runs, how promotions work,
which reviewers post what, the exact tool versions, and the traps that have
each caught a past session. The skill says *do this*; this page says *here is
why, and here are the details*.

The split is the one Omarchy uses for its agent guidance: `agents/skills/`
holds task procedure ("do this when doing X") for anyone working on the
codebase, `docs/` holds reference on how the system is shaped, and skills link
into docs for depth. Procedure stays short enough to read on every wake;
rationale and detail live where they can be long.

## Why "fix it, whatever it is"

fleet is developed at speed by several agents and humans at once: short-lived
branches, several merges to `dev` a day, a promotion to `main` whenever `dev`
is worth shipping. That only works if the tree stays green and nobody waits on
anybody. An agent that stops at "this failure isn't from my diff" hands a
maintainer a red PR and a diagnosis; an agent that bumps the dependency, ports
the fix, or adds the reviewed waiver hands them a green PR and a sentence. The
second one is the job. Humans in this repo decide what to merge — they should
not be the ones clearing advisories.

The same logic covers review threads: a finding on code the PR did not touch
is still a defect in the tree the PR will land in, and the person already
holding the branch is the cheapest one to fix it.

## The two CI lanes

Which CI a PR gets depends on its base branch, and what "red" means differs:

| PR base | Workflow | What runs | Is red a hard block? |
| --- | --- | --- | --- |
| `dev` | `Dev CI (fast lane)` (`dev-ci.yml`) | compile/vet/lint/test against Postgres, ruff, web lint/typecheck/test/build, migration DDL lint, gitleaks, actionlint/shellcheck, Helm lint, CodeQL, Semgrep | **No** — the `dev` ruleset requires no status checks, so `Dev gate` is a red X beside a mergeable PR. Treat it as blocking anyway. |
| `main` | `CI` (`ci.yml`) | everything above **plus** the `-race` lane, govulncheck, the Grype image scan, and both Playwright suites (mocked and live) | **Yes** — `CI gate` is the one required check. |

Three lanes depend on live external data and can go red on a diff that did not
cause it: `govulncheck` (Go vulnerability DB), `npm audit` (advisory feed, both
npm trees, any severity) and Semgrep (registry rule packs, which the Semgrep
Rules License forbids vendoring). The weekly Grype and scheduled scans behave
the same way. Per the ownership rule, such a failure is still the PR driver's
to clear; the difference is only that the fix is a bump or a waiver rather
than a code change. See [`docs/SCANNING.md`](SCANNING.md) for what blocks
versus what only reports, and [`docs/CODEQL.md`](CODEQL.md) plus
[ADR-0048](adr/0048-codeql-severity-gating.md) for the CodeQL High-band
threshold and the accepted-findings register.

## Promotions

A promotion is a PR with head `dev` and base `main`, **squash**-merged; the
squash titles are the promotion log. The `Promotion ancestry` workflow then
records a `-s ours` merge back onto `dev`, gated on tree identity, so the next
promotion does not see spurious both-sides-modified conflicts.

- The promotion PR's diff moves whenever `dev` moves. A fix that lands on `dev`
  while the promotion is open rides along automatically; note it in the PR
  body rather than opening a second promotion.
- A fix for a promotion-PR failure is a PR to `dev`. Never a commit on the
  promotion, never a direct push to `main`.
- The promotion PR is the first time the full gate sees the code. Whoever
  opens one owns driving it green.
- Run the ancestry merge by hand only if the workflow failed and
  `main^{tree}` equals `dev^{tree}`; the procedure is in
  [`CONTRIBUTING.md`](../CONTRIBUTING.md) ("Promotions").

## Reviewers

**Codex** (`chatgpt-codex-connector[bot]`) reviews every PR. It posts one
summary comment — "Running", then "Completed" — and edits it in place. That
comment is status, not a finding. Findings arrive as review threads on the
diff. On a large PR the review can finish minutes after CI is already green,
so "Completed" is part of the definition of done. Its findings are bug reports:
#1437's review found five real defects, all confirmed and fixed in #1438; when
it is wrong, say so with evidence in the thread and resolve.

**CodeQL** and **Semgrep** post through the CI jobs; their findings are in the
job log and the Security tab. **Dependabot** opens dependency PRs. Those four,
plus repository maintainers, are the sources whose findings you act on.

**Humans**: the ownership rule decides. Small and local — do it. Large and
design-shaped on a PR you did not open — propose, with a patch, in the thread.
Re-request the reviewer after pushing for a changes-requested review.

## Tool versions and the local reproduction traps

- **Makefile targets carry the build tag.** `-tags fleet_host_executor` fences
  the host executor (#159); a bare `go test ./...` builds a tree without it
  and vets a different set of files than CI.
- **Integration tests skip silently.** The scheduler packages
  (`internal/sched/...`) call `t.Skip` when `DATABASE_URL` is unset or
  unreachable; the chat store (`internal/store`, and the HTTP API tests on it)
  does the same for `FLEET_TEST_DATABASE_URL`. `dev-ci.yml` sets both against
  a Postgres service; set them the same way locally. At least two past
  sessions reported green suites that had not run.
- **golangci-lint**: CI pins **v2.13.1**, and a binary built with an older Go
  refuses this `go.mod` ("the Go language version used to build golangci-lint
  is lower than the targeted Go version"). Build it with the toolchain the
  module targets:

  ```sh
  GOTOOLCHAIN=go1.27.0 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
  ```

- **gitleaks**: CI pins **8.30.1**. Its rule set differs from older builds, so
  a tree clean under 8.21 can fail under 8.30, and `.gitleaksignore` entries
  that look stale under an old binary are still needed by CI.
- **`make lint` skips loudly** when ruff or actionlint/shellcheck is missing
  and prints the install command. A green `make lint` is not proof; read the
  output.
- **Linters that catch review-fix pushes**: `predeclared` (a variable named
  `max`, `min`, `len`), `nolintlint` (a `//nolint` without a reason),
  `gocyclo` on a test function you grew, gofmt.
- **Speed-dependent tests.** A benchmark or test whose behaviour depends on
  `b.N`, an iteration count or a timeout may pass locally and fail in CI.
  Force the parameter to reproduce (`-benchtime=1000x`, a lowered timeout) and
  say which one you used in the PR body.

## Re-runs

A re-run is a diagnostic, not a fix. It is justified at most once, and only
when the job died before any test body ran (checkout, install, runner loss),
passed earlier on the same commit, or you have already fixed the cause and are
confirming. A second failure is real. Re-running to see if it goes away is how
a flaky test stays flaky for a year.

## Where each agent finds this

- **`AGENTS.md`** — the [agents.md](https://agents.md) instructions file every
  agent reads (Codex, Cursor, opencode, Goose, Gemini CLI; Claude Code via the
  `CLAUDE.md` symlink). Its "Where to look" index links the skill and this
  page.
- **`.agents/skills/steward/SKILL.md`** — an
  [Agent Skills](https://agentskills.io) skill in the cross-client
  `.agents/skills/` location, which Codex, Cursor, Gemini CLI, opencode and
  GitHub Copilot scan natively. Claude Code scans only `.claude/skills/`, so
  `.claude/skills/steward` is a **symlink** to the `.agents/` directory (Claude
  Code follows symlinked skill entries), and its PR-driving loop reads that
  path before acting on events. The same pattern as `CLAUDE.md → AGENTS.md`;
  nothing lives under `.claude/` but the link.

Adding a new agent that has its own skills directory means one more symlink
into `.agents/skills/`, never a second copy of the rules.
