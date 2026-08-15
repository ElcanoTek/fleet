# ADR-0044: Remove the in-sandbox browser tool; browser automation is a connector

- **Status:** Accepted
- **Date:** 2026-08-15
- **Deciders:** fleet maintainers
- **Supersedes:** [ADR-0022](0022-governed-browser-tool.md)

## Context

[ADR-0022](0022-governed-browser-tool.md) shipped a native `browser` tool (#503,
v1 mode-1): a Playwright(sync) session living in the per-conversation Python
kernel, gated by `FLEET_BROWSER_ENABLED` (default off), with egress — not
per-action approval — as the security boundary.

What the shipped tool actually cost, measured against what it delivered:

- **It could not run on the image fleet ships.** Chromium + Playwright were
  deliberately kept out of `config/default/sandbox/Containerfile` (weight plus a
  CVE surface the CI Grype gate fails on), so every deployment that wanted the
  browser had to add and pin the layer in its own client-config Containerfile.
  Out of the box the tool answered `BROWSER_NOT_INSTALLED`.
- **The real Chromium path was never exercised in CI.** ADR-0022's own honest
  scope says so: snippet generation, gating, and result parsing were unit-tested;
  Chromium-in-rootless-Podman (with its `/dev/shm` and user-namespace friction)
  was left to per-deployment verification. An untested path in the security-
  critical sandbox is a liability, not a capability.
- **DOM-only v1 was the whole of it.** No vision, no credentials, no login-walled
  sites, interactive turns only. The mode-2 follow-on (human-authorized local
  browser operator) was never built, and the capability gap it left is exactly
  the one users hit first.
- **Hosted browser-automation connectors now cover the use case.** Browserbase
  and peers reach fleet as remote MCP servers through the existing connector
  directory and the host-side credential broker — bundle data, not engine code,
  which is what the repo-boundaries doctrine in `AGENTS.md` asks for. They also
  handle the parts v1 punted on (sessions, logins, CAPTCHA handoff).

Keeping a permanently-off, image-dependent, CI-unverified tool in the engine to
occupy a capability slot a connector fills better is the wrong trade.

## Decision

**Delete the native `browser` tool and everything that gated it.** Concretely:

1. `internal/tools/browser.go`, `browser_preamble.go`, and `browser_test.go` are
   removed. `NewTurnTools(sb)` loses its variadic-option seam (the browser was
   its only option) and returns the fixed interactive roster.
2. `config.BrowserEnabled` / `FLEET_BROWSER_ENABLED` are removed, including the
   `.env` allowlist entry — a leftover setting in an operator's env file is now
   ignored rather than silently doing nothing.
3. The commented Chromium/Playwright reference layer is dropped from
   `config/default/sandbox/Containerfile`. A deployment that still wants a
   browser in its own sandbox image is free to install one; fleet no longer
   documents or ships a tool that drives it.
4. `browser` leaves the `role=explore` sub-agent write-denylist
   (`exploreDeniedNativeTools`, ADR-0007 / #1043) and its pinning test. The
   denylist stays exhaustive over the tools that actually exist — a stale entry
   for a deleted tool would make the pinning test weaker, not stronger.
5. Browser automation, where a deployment wants it, arrives as an MCP connector
   (e.g. Browserbase, #987) under the existing remote-MCP posture: host-side
   credential brokering ([ADR-0003](0003-host-side-mcp-credential-brokering.md)),
   child-side scope authorization
   ([ADR-0042](0042-child-side-mcp-scope-authorization.md)), and the per-user
   remote-MCP runtime ([ADR-0040](0040-child-owned-remote-mcp-runtime.md)).

**No sandbox invariant is weakened by this change** — it only removes a
capability. The mandatory sandbox, the lockdown seal, and the egress postures
are untouched; there is simply one less kernel-resident consumer of them.

## Consequences

- Deployments that had set `FLEET_BROWSER_ENABLED=true` lose the `browser` tool
  at upgrade. The var is no longer read; the CHANGELOG names the connector route
  as the replacement. This is a **breaking change for those deployments** and is
  called out as such rather than folded in silently.
- fleet again has no first-party way to drive an API-less site without a
  connector. That is the honest state: it is better than a tool that answers
  `BROWSER_NOT_INSTALLED` on the shipped image.
- If a first-party browser is ever wanted again, this ADR is the one to
  supersede, and ADR-0022's design analysis (egress-not-approvals, the
  submit-gate-is-theater argument) remains the correct starting point.

## Alternatives rejected

- **Keep it, ship Chromium in the default image.** Adds weight and a standing
  CVE surface to the image every fleet deployment runs, to enable a tool most of
  them will not use — and the Grype gate would fail on the first fixable
  CRITICAL in Chromium.
- **Keep it, add the CI e2e that was deferred.** Real cost (a Chromium-in-
  rootless-Podman lane) for a DOM-only v1 that still has no credentials and no
  login-walled coverage — the capability users actually ask for.
- **Keep it dormant** (registered but off, undocumented). Dead code in the
  sandbox path that nobody exercises is precisely what rots into a security
  problem; "off by default" is not a maintenance plan.
