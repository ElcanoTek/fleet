# ADR-0048: CodeQL gates on High-and-above plus a reviewed accepted-findings register

- **Status:** Accepted
- **Date:** 2026-08-22
- **Deciders:** fleet maintainers
- **Amends:** the gating decision shipped in #1246 (`docs/CODEQL.md`,
  `docs/SCANNING.md`) — the CodeQL threshold changes from *any finding* to
  *High-and-above, minus a reviewed register*. No other scanner's threshold
  changes.

## Context

#1246 restored CodeQL as **advanced setup** running `security-extended` over
go / python / javascript-typescript / actions, added a `Fail on findings` step,
and routed the result into `ci-gate` / `Dev gate` through `workflow_call`. All of
that was right and none of it is revisited here.

The **threshold** was wrong, and it was wrong for an instructive reason.

The step failed the job on *any* finding at any severity. Its own comment
recorded the justification:

> Threshold is ANY finding, deliberately. The security suite currently reports
> ZERO across go/python/javascript-typescript/actions, so there is no backlog to
> grandfather and no severity line to argue about — a finding here is new.

That zero was real, and it was measured — on Dev CI run 525, a `pull_request`
event. On `pull_request` events the CodeQL action runs **diff-informed**: it
builds the full database and evaluates every query, then reports only results
whose location falls inside the PR's diff. Run 525's own log says both halves
out loud:

```
Computing PR diff ranges...
Persisted 204 diff range(s) across 43 file(s).
Successfully created diff range extension pack at .../pr-diff-range
codeql database run-queries ... --extension-packs=codeql-action/pr-diff-range
```
```
To speed up pull request analysis, file coverage information is only enabled
when analyzing the default branch and protected branches.
```

The Go database held all 428 files and the queries that later fired did run —
`LogInjection.ql`, `TaintedPath.ql`, `RequestForgery.ql`,
`WeakSensitiveDataHashing.ql` are all listed as "Interpreted" in that run. The
SARIF was empty because the results were filtered to the PR's 43 changed files.

So the first full-tree evaluation of `security-extended` against this repository
was the **push** that merged #1246: Dev CI run 527, which reported **38 Go and 17
javascript-typescript findings** and turned `Dev gate` red. The gate then blocked
every subsequent push to `dev` — including any push that would have fixed it —
with no PR-shaped path out, because a PR into `dev` is scanned diff-informed and
therefore green while `dev` itself stays red.

Two conclusions, and the second is the one that generalises:

1. The any-finding threshold was never armed over a clean tree. It was armed over
   a 55-finding backlog that no one had measured, because the only measurement
   available at PR time is structurally incapable of showing it.
2. **A PR-event CodeQL run cannot certify a tree.** It certifies a diff. Any
   claim of the form "the scanners are green, therefore the tree is clean" that
   rests on a `pull_request` run is unsound, and that is a permanent property of
   diff-informed analysis, not a bug to be fixed.

The 55 were then triaged individually against the code. Four were reachable:

- `internal/sched/handlers/handlers.go` logged `task.Prompt` unsanitized on the
  task-create path, while the **update** path's twin line was already wrapped in
  `logSafe`. `POST /tasks` is reachable by a scoped `create_task` key, so this
  was genuine log forgery — and demonstrably so.
- `internal/httpapi/attachments.go` logged the raw client attachment path with
  `%s` on the two branches where the containment guard had just *failed*, i.e.
  precisely where the value is hostile by construction.
- `internal/agent/session.go` logged the client-echoed attachment `Name`, which
  — unlike `Path` — is never re-sanitized on the `/chat` path.
- `web/e2e/test-auth-key.ts` wrote an Ed25519 private key to a fully predictable
  path in the world-writable temp dir at default `0644`.

Those four are fixed in code. The remaining 51 are false positives in fleet's
threat model, and the interesting part is that **severity alone does not separate
them**: `go/request-forgery` carries security-severity 9.1 and fires on
`web_fetch.go`, which is a deliberate user-facing fetch tool sitting behind
`internal/netguard`'s resolve-then-dial SSRF guard. `go/weak-sensitive-data-hashing`
carries 7.5 and fires on SHA-256 used as a lookup index over a 32-byte
`crypto/rand` token — the recommended construction. A pure severity line would
block both.

## Decision

**CodeQL blocks on a finding that is (a) at SARIF level `error`/`warning`, or has
security-severity >= 7.0, and (b) is not waived.** Findings below that band are
printed and uploaded to the Security tab as advisory. Waivers come from two
places:

1. `.github/codeql-accepted-findings.json` — a register of accepted
   `(rule, file)` pairs, each with a mandatory written reason.
2. An in-source `// codeql[rule-id]` comment, which CodeQL emits as a
   `suppressions` array on the result. (Both `go` and `javascript` ship an
   `AlertSuppression.ql`; the comment must sit on its own line and covers the
   line immediately below it.)

The register is **per-file, not per-rule**, and that is the whole point of
preferring it to a `query-filters` exclude. A `query-filters: exclude: {id:
go/request-forgery}` switches a security-severity 9.1 query off for the entire
repository; the register waives it in `internal/tools/web_fetch.go` and
`internal/mcpoauth/discovery.go` and leaves it live everywhere else, including
elsewhere in those same packages. This is asserted, not asserted-and-hoped: a
synthetic SARIF carrying a fresh `go/request-forgery` in an unregistered file
fails the gate, and that case is exercised as part of validating the jq.

Three anti-rot controls, because a waiver register that nobody re-reads is worse
than no register:

- `scripts/check_codeql_register_test.go` (in `make test`) requires every entry
  to name a file that exists, carry a substantive reason, use a plausible rule
  id, and be unique — and asserts that `codeql.yml` still references the register
  at all, so the two cannot be silently decoupled.
- The gate **fails closed** if the register is missing, and fails closed if the
  jq cannot be evaluated. A scan that could not be judged is never reported clean.
- The job log and step summary print the **ACCEPTED tier by name**, alongside
  BLOCKING and ADVISORY, so every waiver is visible in ordinary CI output rather
  than only in a file somebody has to think to open.

## Consequences

**What gets better.** `dev` and `main` are unblocked, and for the first time the
push-event runs report a verdict that means something: the High-and-above band is
enforced tree-wide, on every push, with a reviewed exception list. The
`security-extended` suite keeps running in full — nothing is filtered out of the
Security tab — so the 51 advisory/accepted findings remain visible for triage.
The specific claim "a green CodeQL check means the tree is clean" is now
false-by-construction only for PR events, and the docs say so instead of implying
otherwise.

**What gets worse.** A note-level regression no longer fails the build. If
someone adds a 23rd `go/log-injection` sink on genuinely untrusted input, CI will
not stop them; it will appear in the advisory tier and in the Security tab. This
is a deliberate trade: the alternative, as demonstrated above, is a gate that
blocks every push and therefore gets routed around or switched off. `gosec`'s
G706 covers the same log-injection class in `golangci-lint`, which **does** block
via `ci-gate`, and carries 77 reviewed per-site annotations — so this class is
not unguarded, it is guarded by the instrument that was already there.

**What is now load-bearing.** Widening the register is a security decision that
shows up in a PR diff, and reviewers are expected to check the reason against the
code rather than the reason's existence. That is a process control, and process
controls decay; the tests above are what make the decay visible.

**Known limitation, stated rather than fixed.** The 621 `_test.go` files remain
outside the Go database (autobuild builds packages, not tests) — unchanged from
default setup and from #1246. And the register keys on `(rule, file)` rather than
`(rule, file, line)` deliberately: line numbers churn on every edit, and a
register that fails on unrelated refactors is a register people delete. The cost
is that a *second*, genuinely bad instance of an already-waived rule in an
already-waived file would not block. That is the sharpest edge here, and it is
the reason the reason-strings name the specific call sites and their guards.
