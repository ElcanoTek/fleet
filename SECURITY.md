# Security Policy

We take the security of fleet seriously. Thank you for helping keep fleet and
its users safe.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
pull requests, or discussions.**

Instead, report them privately by email to **brad@elcanotek.com**.

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

## CSRF protection (cookie-authenticated routes)

State-mutating orchestrator routes (`POST /tasks`, `POST /upload`, …) accept the
shared `elcano_auth` session cookie. Cookie-authenticated requests are protected
against cross-site request forgery by a **stateless Origin check**
(`CSRFMiddleware`, applied globally before every route group): a mutating request
on the cookie path must carry an `Origin` header whose host matches the server's
(`X-Forwarded-Host` when behind a proxy, else `Host`). A missing, malformed, or
cross-origin `Origin` is rejected with `403 Cross-origin request blocked`.

Requests authenticated with `X-API-Key`, `X-Registration-Token`, or
`Authorization: Bearer …` are **exempt** — browsers do not auto-attach custom
headers cross-origin, so those paths are not CSRF-reachable.

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
- Its `mcp/*.py` servers run as **host-side subprocesses** with the per-account
  brokered MCP credentials placed in their environment.

So the README's "credentials never enter the sandbox" guarantee is about the
*sandbox* — brokered secrets **do** reach the bundle's own host-side MCP servers
by design. Anyone who can push to the bundle's tracked branch gains host-side
code execution under the fleet service identity and access to those secrets on
the next `update`. Protect the bundle repo accordingly: restrict who can push,
require signed commits / branch protection, and pin the checkout to a reviewed
commit when you can.

## Scope

This policy covers the code in this repository. Deployments are configured by a
separate, operator-supplied client-config bundle and environment file; the
security of a given deployment also depends on how those are managed (see above).
