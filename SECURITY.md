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

## Scope

This policy covers the code in this repository. Deployments are configured by a
separate, operator-supplied client-config bundle and environment file; the
security of a given deployment also depends on how those are managed.
