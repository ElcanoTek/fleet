# PR stewardship — the reference

The **procedure** for driving a fleet pull request to green after it is opened
lives in the skill at
[`.agents/skills/steward/SKILL.md`](../.agents/skills/steward/SKILL.md): read
that first, and read it before acting on any PR event. This page is the
reference behind it — what CI actually runs, how a merge becomes a release,
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
branches, several merges to `main` a day, and every green merge is a release
([ADR-0062](adr/0062-trunk-based-development.md), ADR-0059). That only works if
the tree stays green and nobody waits on anybody. An agent that stops at "this
failure isn't from my diff" hands a
maintainer a red PR and a diagnosis; an agent that bumps the dependency, ports
the fix, or adds the reviewed waiver hands them a green PR and a sentence. The
second one is the job. Humans in this repo decide what to merge — they should
not be the ones clearing advisories.

The same logic covers review threads: a finding on code the PR did not touch
is still a defect in the tree the PR will land in, and the person already
holding the branch is the cheapest one to fix it.

## The one CI lane

Every PR targets `main` and gets the same workflow, `CI` (`ci.yml`): Go
build/vet/lint/test against Postgres including the `-race` lane, govulncheck,
ruff, web lint/typecheck/test/build, both Playwright suites (mocked and live
against a real backend + sandbox), the Grype image scan, Helm lint, migration
DDL lint, gitleaks, actionlint/shellcheck, CodeQL and Semgrep. `CI gate` is the
one required status check, so red is always a hard block; a docs-only PR skips
the heavy jobs via the `changes` classifier and the gate accepts exactly that
skip. Expect a full run to take about 25 minutes, most of it the `-race` lane.

Three jobs depend on live external data and can go red on a diff that did not
cause it: `govulncheck` (Go vulnerability DB), `npm audit` (advisory feed, both
npm trees, any severity) and Semgrep (registry rule packs, which the Semgrep
Rules License forbids vendoring). The weekly Grype and scheduled scans behave
the same way. Per the ownership rule, such a failure is still the PR driver's
to clear; the difference is only that the fix is a bump or a waiver rather
than a code change. See [`docs/SCANNING.md`](SCANNING.md) for what blocks
versus what only reports, and [`docs/CODEQL.md`](CODEQL.md) plus
[ADR-0048](adr/0048-codeql-severity-gating.md) for the CodeQL High-band
threshold and the accepted-findings register.

## Merging is releasing

Every PR is **squash**-merged into `main`, and every green push to `main` is
tagged and published by `release.yml` (ADR-0059). The squash commit message —
prefilled from the PR title and body — is the release notes, verbatim, ahead
of GitHub's generated list (ADR-0061). So the PR body's "What changed, and
why" is written for the operator who reads the release, and the merge dialog
is where the checklist, the verification section and any tool footer get
dropped. A fix for a red `main` is the next PR; nothing is ever pushed to
`main` directly and history is never rewritten there.

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
  and vets a different set of files than CI. `make test` runs
  `scripts/go-test.sh`, which serializes only the packages that share a
  Postgres DSN — a global `-p 1 ./...` is the slow path this script exists to
  avoid.
- **Integration tests skip silently.** The scheduler packages
  (`internal/sched/...`) call `t.Skip` when `DATABASE_URL` is unset or
  unreachable; the chat store (`internal/store`, and the HTTP API tests on it)
  does the same for `FLEET_TEST_DATABASE_URL`. `ci.yml`'s `go` job sets both
  against a Postgres service; set them the same way locally. At least two past
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
  that look stale under an old binary are still needed by CI. A gitleaks
  finding is the one scanner result the ownership rule does **not** let you
  clear with a waiver by default: `.gitleaksignore` is for values verified to
  be fake fixtures or false positives, with the verification written next to
  the entry. A real credential is removed and treated as compromised — rotated
  by whoever owns it, or reported privately per `SECURITY.md` — and the PR
  says so without quoting the value. "No secrets in the repo" is an
  `AGENTS.md` invariant; a green scanner over a live secret violates it twice.
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

Adding an agent that scans only its own directory means one more symlink
**in that agent's directory** pointing back at `.agents/skills/steward` —
exactly the shape of the Claude one above — never a second copy of the rules,
and never a link placed inside `.agents/skills/`, which already holds the
canonical skill.
