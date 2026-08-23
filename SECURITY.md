# Security Policy

We take the security of fleet seriously. Thank you for helping keep fleet and
its users safe.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
pull requests, or discussions.**

Instead, report them privately by email to **hello@elcanotek.com**.

Please include as much of the following as you can, so we can triage quickly:

- A description of the vulnerability and its potential impact.
- Steps to reproduce, or a proof-of-concept.
- The affected component(s) and version / commit.
- Any suggested remediation, if you have one.

If you would like to encrypt your report, mention that in an initial email and
we will arrange a secure channel.

## What to expect

- **Acknowledgement** within 3 business days of your report.
- **An initial assessment** (severity and likely remediation path) within
  7 business days.
- **Progress updates** as we work on a fix, and credit in the release notes once
  the issue is resolved — unless you prefer to remain anonymous.

We ask that you give us a reasonable opportunity to remediate the issue before
any public disclosure.

## Supported versions

fleet is pre-1.0 and under active development. Only the latest `main` is
supported — fixes land on `main` and there are no maintained release branches
yet. Please reproduce against current `main` before reporting.

## Secret scanning

CI runs [gitleaks](https://github.com/gitleaks/gitleaks) — `gitleaks dir .
--redact --exit-code 1`, over the whole working tree, not just the diff — and
fails the build on any new, un-ignored secret. It is the one lane that is
deliberately **not** gated on the docs-only classifier, because a secret can be
pasted into a markdown file.

To be precise about coverage, since "on every push" would overstate it: the scan
runs on **pull requests into `dev` and `main`, and on pushes to `dev` and
`main`**. Those are the only events either CI workflow subscribes to, so a push
to a personal feature branch runs no CI at all — including no secret scan — until
a PR is opened against `dev`. Treat pre-PR local hygiene accordingly.

If you are contributing, never commit real credentials — the generic
`config/default` bundle ships with no connector secrets, and all deployment
secrets live in an operator-managed `0600` env file outside the repo (see the
README).

## Static analysis (SAST)

Two static analyzers gate merges, and an auditor will want them by name. The full
design notes are [`docs/SCANNING.md`](docs/SCANNING.md) (who checks what, and what
actually gates) and [`docs/CODEQL.md`](docs/CODEQL.md); the threshold decision is
[ADR-0048](docs/adr/0048-codeql-severity-gating.md).

**CodeQL — advanced setup, four languages, `security-extended`.**
`.github/workflows/codeql.yml` analyzes `go`, `python`, `javascript-typescript`
and `actions`, each with the `security-extended` query suite (the broader security
set, not the default one). Go builds via `autobuild` with
`GOFLAGS=-tags=fleet_host_executor`, so `internal/sandbox/host.go` — the
unsandboxed host executor, fenced out of the default build — is inside the
database rather than the one file the analysis cannot see. It is a **reusable**
workflow: `ci.yml` (main) and `dev-ci.yml` (dev) call it as a job, so its result
lands in the caller's aggregate gate. There is also a weekly schedule, because a
CodeQL verdict is a function of the query pack as well as the commit.

Four properties of that gate matter for an audit, and each is a deliberate
limit rather than an oversight:

- **The threshold is the High band, not "any finding".** A finding blocks when its
  rule publishes `security-severity >= 7.0` (CodeQL's own High/Critical cut), or —
  for a rule that publishes no security-severity — when its SARIF level is
  `error`/`warning`. Level is deliberately not used for rules that *do* publish a
  security-severity: nearly every CodeQL security query is
  `@problem.severity error`, `go/log-injection` at severity 6.1 included, so
  banding on level would block on everything. Findings below the band are
  **advisory** — printed in the job log and uploaded to the Security tab, not
  blocking.
- **Accepted findings are a reviewed register, not a disabled query.**
  `.github/codeql-accepted-findings.json` holds accepted `(rule, file)` pairs,
  each with a mandatory written reason that must say why the finding is not
  exploitable *there*. It is per-**file** on purpose: a `query-filters` exclude
  would switch a security-severity 9.1 query off repo-wide, whereas a register
  entry leaves it live everywhere else. An in-source `// codeql[rule-id]` comment
  is the second waiver route. Widening the register appears in the PR diff, and
  `scripts/check_codeql_register_test.go` fails the test suite on an entry that
  names a missing file, lacks a reason, or has become decoupled from the workflow.
- **A `pull_request` run certifies a diff, not a tree.** On PR events the CodeQL
  action runs **diff-informed**: it builds the full database and evaluates every
  query, then reports only results inside the PR's diff. Tree-wide verdicts come
  only from the push and scheduled runs. Any "the scanners are green, therefore
  the tree is clean" claim resting on a PR run is unsound — this repo learned that
  the expensive way, and ADR-0048 records it.
- **`_test.go` files are outside the Go database.** `autobuild` builds packages,
  not tests, so every `_test.go` file in the tree (625 at the time of writing) is
  unanalyzed. Unchanged from GitHub's default setup; bringing them in would
  require `build-mode: manual`.

The gate **fails closed**: a missing register, a missing filter file, unparseable
SARIF, or findings present with zero rule metadata resolved all fail the job
rather than reporting clean. One shared classifier (`.github/codeql-gate.jq`)
feeds both the report and the gate, so the two cannot disagree, and the job log
prints three tiers — BLOCKING, ACCEPTED (by name), ADVISORY. Note that
**dismissing an alert in the Security tab does not turn the check green**: the
gate reads the run's own SARIF and never consults the code-scanning API.

**Semgrep — all four registry packs, blocking on any finding.**
`.github/workflows/semgrep.yml` runs `p/github-actions`, `p/golang`,
`p/javascript` and `p/python` with `--error` and no `continue-on-error`, also as a
reusable workflow inside both gates, also with a weekly schedule. The tree is at
zero unsuppressed findings; six false positives are waived at the line with
`nosemgrep: <rule-id>` plus a reason, and each waiver was mutation-tested
(removing it makes the finding reappear, so a green scan means the waivers work
rather than the rules having silently stopped matching). One honest limitation:
the rule packs are fetched from the registry at scan time and cannot be vendored —
the Semgrep Rules License v1.0 forbids redistribution, and this is a public MIT
repo — so a registry-side rule addition can turn the lane red with no commit to
blame. The binary version is pinned; the rules are not.

**All 53 third-party action references across the 13 workflow files are pinned to
40-hex commit SHAs** with the version in a trailing comment, which is also the
form Dependabot updates. Two of those pins were subtly wrong — taken from the
*annotated tag object* of a mutable major tag rather than the commit it points at,
so they looked like commit pins while resolving to a moving tag — and both are now
the peeled commit, with `scripts/check_action_pins_test.go` asserting the shape.

**Where enforcement actually lands.** All of the above is wired into the branches'
aggregate gate jobs, but a gate job only blocks a merge where it is a *required*
status check. `CI gate` is required on `main`. The `dev` ruleset requires no
status checks at all, so `Dev gate` — and every scanner inside it — is
red-but-not-required on `dev`. That gap, and what it interacts with, is written up
under "Known gaps" in [`docs/SCANNING.md`](docs/SCANNING.md).

## Supply-chain security (dependencies)

Fleet pulls third-party code from three ecosystems — Go modules at the repo root,
npm packages under `web/` and under `scripts/rampart-service`, and Fedora RPMs
inside the sandbox image — and relies on several deliberate controls to keep a
compromised or fresh-and-unvetted release from reaching `main`:

- **Go module integrity is verified, with the defaults intact.** The repo commits
  a complete `go.sum`, and the build does **not** set any of `GOFLAGS`, `GOPROXY`,
  `GOSUMDB`, `GONOSUMCHECK`, `GONOSUMDB`, `GOPRIVATE`, or `GOINSECURE` — so Go's
  out-of-the-box posture applies: modules are fetched through the public proxy
  (`proxy.golang.org`) and every download is checked against the committed
  `go.sum` and the public checksum database (`sum.golang.org`). The checksum DB
  is **not** disabled. This is a conscious choice: there is no private module
  source to exempt, so weakening these (e.g. `GONOSUMCHECK`, `GOFLAGS=-insecure`,
  or a `GOPRIVATE` carve-out) would only remove protection. Do not add such an
  override without a documented reason and a corresponding entry here.
- **Dependency-CVE scanning.** CI runs `govulncheck` against the Go module on
  every PR (the `govulncheck` job in `.github/workflows/ci.yml`), failing the
  build on a known-vulnerable dependency that fleet actually calls into.
- **npm dependency-CVE scanning.** `npm audit --audit-level=low` runs in the
  `web` job of **both** CI lanes, lockfile-only (before the install, so a
  vulnerable lockfile fails fast) and against **both** npm trees — `web/` and
  `scripts/rampart-service` — failing the build on **any** severity. Like
  govulncheck, its verdict is a function of the clock as well as the commit: a
  newly published advisory can redden an unchanged tree, which is the point.

  Two `overrides` in `scripts/rampart-service/package.json` are load-bearing and
  worth disclosing: `sharp ^0.35.3` and `adm-zip ^0.6.0`, each the release
  immediately after a vulnerable range that **no upstream release yet fixes**
  (`@huggingface/transformers` still pins `sharp ^0.34.5`; `adm-zip` arrives
  under `onnxruntime-node`). An override is a fork of upstream's intent, correct
  only while upstream is broken — so `scripts/check-npm-overrides.sh` runs beside
  the audit in both lanes and **fails the build with removal instructions the day
  upstream's own ranges reach the patched lines**. A registry flake skips with a
  notice rather than delivering a verdict; the audit above is the CVE gate.
- **Container-image CVE scanning.** CI also scans the rootless-Podman sandbox
  image (built from `config/default/sandbox/Containerfile`) with Grype in the
  `grype-scan` job, a surface `govulncheck` (Go modules only) cannot see.
  `scripts/check-grype-policy.sh` fails the build on a *fixable* **CRITICAL or
  HIGH** CVE — and only in the image's **RPM** packages. Be precise about that
  restriction, because it is a real, deliberate limit: Grype also catalogs the
  Python `dist-info` that Fedora RPMs ship as independent PyPI artifacts, and
  those records use upstream versions and advisories, so one can claim a fix
  exists when Fedora has already backported it or has not published an RPM
  update. Those language records are **uploaded to SARIF but do not gate**;
  treating them as a merge gate previously produced hand-maintained pip
  replacements layered over a coherent distro package set. MEDIUM and below are
  reported, not blocking. Findings upload to GitHub Security → Code scanning
  (category `grype-sandbox-image`), and a weekly scheduled scan
  (`.github/workflows/grype-scheduled.yml`) catches newly-disclosed CVEs against
  the existing image between PRs.

  Scope note: `grype-scan` lives only in `ci.yml`, so it runs on main-targeting,
  non-docs-only events. PRs into `dev` get no image scan; the image is scanned at
  the dev→main promotion and weekly.
- **Release cooldown — on the two ecosystems that support it.**
  `.github/dependabot.yml` applies a `cooldown` to the gomod and npm surfaces so
  Dependabot waits a few days (3 for patch, 7 for minor, 14 for major) before
  proposing a freshly published release. This blunts fast typosquat /
  account-takeover attacks, where a malicious version is published and then yanked
  once the ecosystem flags it. Cooldown applies to version updates only — Dependabot
  **security** updates are never delayed, so urgent CVE fixes still flow
  immediately.

  **The `github-actions` ecosystem is the exception, and it is the one where a
  cooldown would matter most.** Dependabot supports `cooldown` for gomod and npm
  only, so the one ecosystem whose "dependency" is *the CI definition itself* —
  a `github-actions` bump rewrites `.github/workflows/*` and therefore changes
  what CI executes — cannot be made to wait, and it is configured daily against
  `dev`, which additionally has no required status checks (see "Static analysis"
  above). What contains that combination now is simply that **every** Dependabot
  PR takes a human: automatic merging was removed from this repository, so no
  dependency bump of any ecosystem or bump level reaches a branch without someone
  looking at it.

The cooldown reduces the window for a fast attack but is **not** a guarantee:
a patient attacker who waits out the cooldown, or a compromise the ecosystem
never flags, would still slip through. The committed `go.sum` + checksum DB,
`govulncheck` and `npm audit` are the stronger, always-on controls; the cooldown
is defense-in-depth on top of human review, and it does not cover
`github-actions` at all.

## CSRF protection (cookie-authenticated routes)

State-mutating orchestrator routes (`POST /tasks`, `POST /upload`, …) accept the
shared `elcano_auth` session cookie. Cookie-authenticated requests are protected
against cross-site request forgery by a **stateless Origin check**
(`CSRFMiddleware`, applied globally before every route group): a mutating request
on the cookie path must carry an `Origin` header whose host matches the server's
(`X-Forwarded-Host` when behind a proxy, else `Host`). A missing, malformed, or
cross-origin `Origin` is rejected with `403 Cross-origin request blocked`.

Requests authenticated with `X-API-Key`, `X-Registration-Token`,
`Authorization: Bearer …`, or the Next-proxy `X-Orchestrator-Server-Token` are
**exempt** — browsers do not auto-attach custom headers cross-origin, so those
paths are not CSRF-reachable. On the proxy path the browser's Origin has
already been enforced by the Next.js layer (`web/src/app/lib/csrf.ts`), which
does not forward the header upstream.

Two operator-facing contracts make this defense-in-depth complete:

- **The auth service MUST set `SameSite=Lax` (or `Strict`) on the `elcano_auth`
  cookie.** Fleet reads and deletes that cookie but does not mint it; `SameSite`
  is the browser's first line of defense and blocks the overwhelming majority of
  CSRF vectors before any server check runs. The Origin check is the backstop.
- **Non-browser clients that authenticate with the `elcano_auth` cookie must set
  `Origin` explicitly** on every `POST`/`PUT`/`DELETE`/`PATCH`, e.g.
  `Origin: https://fleet.example.com`. Requests that omit it receive `403`.
  Clients using `X-API-Key` / `X-Registration-Token` / `Authorization: Bearer`
  are unaffected.

## The client-config bundle is root-equivalent

A deployment's behavior comes from an external **client-config bundle** (a git
repo `FLEET_CLIENT_CONFIG_DIR` points at). Treat write access to that repo as
**production access**, because the bundle is effectively root-equivalent on the
box:

- Its `sandbox/Containerfile` is **built and run** on the host.
- Its MCP servers run as **host-side subprocesses of the dedicated MCP broker**
  with the selected per-account credentials placed in their environment.

So the README's "credentials never enter the sandbox" guarantee is about the
*sandbox* — brokered secrets **do** reach the bundle's own host-side MCP servers
by design. Anyone who can push to the bundle's tracked branch gains host-side
code execution under the fleet service identity and access to those secrets on
the next `update`. Protect the bundle repo accordingly: restrict who can push,
require signed commits / branch protection, and pin the checkout to a reviewed
commit when you can.

## What the MCP broker boundary does and does not protect

Connector execution runs in a separate `fleet mcp-broker` process, and the agent
loop's process scrubs every connector environment key once that child is verified
up. Two things are worth stating plainly about the shape of that boundary, so it
is not read as more than it is.

**What it protects.** After boot, the agent-loop process cannot read a bundle
connector secret, and it cannot exercise one past the gates: the credential owner
re-derives each server's tool allowlist and enabled-server set from its own copy
of the bundle, refuses a scope selection naming a server it does not have, and
refuses a call outside the scope's bound set — so a gating bug in the agent-loop
process restricts a run rather than unbounding it. See ADR-0042 and
`docs/MCP-BROKER-SCOPES.md`.

**What it does not protect: remote-MCP OAuth tokens at rest.** Per-user hosted
MCP *runs* resolve their tokens in the child (ADR-0040), but the OAuth **control
plane** — connect, callback, and connection CRUD — is parent-side by design, and
the parent therefore installs the at-rest cipher on the chat store. Code running
in the parent process can decrypt any user's stored remote-MCP tokens. This is an
**accepted boundary**, not an oversight: browser redirects and user credential
intake are host control-plane operations, they are not model-callable, and moving
them behind the child would require a second authenticated control protocol
without changing where model-initiated MCP calls execute.

The threat model that follows is explicit: **compromise of the fleet parent
process implies compromise of stored remote-MCP tokens** (and of the other
secrets the same store cipher protects). The broker boundary is an
address-space-isolation claim about *connector credentials and agent-initiated
calls*, not a claim that a fully compromised parent cannot reach the database or
its encryption key. Full control-plane isolation is a possible future change,
tracked separately rather than left ambiguous.

## Scope

This policy covers the code in this repository. Deployments are configured by a
separate, operator-supplied client-config bundle and environment file; the
security of a given deployment also depends on how those are managed (see above).
