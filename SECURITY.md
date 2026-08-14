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

CI runs [gitleaks](https://github.com/gitleaks/gitleaks) on every push and pull
request and fails the build on any new, un-ignored secret. If you are
contributing, never commit real credentials — the generic `config/default`
bundle ships with no connector secrets, and all deployment secrets live in an
operator-managed `0600` env file outside the repo (see the README).

## Supply-chain security (dependencies)

Fleet pulls third-party code from two ecosystems — Go modules at the repo root
and npm packages under `web/` — and relies on several deliberate controls to
keep a compromised or fresh-and-unvetted release from reaching `main`:

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
- **Container-image CVE scanning.** CI also scans the rootless-Podman sandbox
  image (built from `config/default/sandbox/Containerfile`) with Grype on every
  non-docs PR (the `grype-scan` job), failing the build on a *fixable* CRITICAL
  CVE in the image's RPM or Python packages — a surface `govulncheck` (Go modules
  only) cannot see. Findings upload to GitHub Security → Code scanning, and a
  weekly scheduled scan (`.github/workflows/grype-scheduled.yml`) catches
  newly-disclosed CVEs against the existing image between PRs.
- **Release cooldown.** `.github/dependabot.yml` applies a `cooldown` to the gomod
  and npm surfaces so Dependabot waits a few days (3 for patch, 7 for minor, 14
  for major) before proposing a freshly published release. This blunts fast
  typosquat / account-takeover attacks, where a malicious version is published and
  then yanked once the ecosystem flags it. It matters most for **patch** bumps,
  which `.github/workflows/auto-merge-dependabot.yml` auto-merges once the full CI
  gate is green: without a cooldown a minutes-old patch could be proposed and
  auto-merged before any scrutiny. Cooldown applies to version updates only —
  Dependabot **security** updates are never delayed, so urgent CVE fixes still
  flow immediately.

The cooldown reduces the window for a fast attack but is **not** a guarantee:
a patient attacker who waits out the cooldown, or a compromise the ecosystem
never flags, would still slip through. The committed `go.sum` + checksum DB and
`govulncheck` are the stronger, always-on controls; the cooldown is
defense-in-depth on top of the auto-merge path.

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
