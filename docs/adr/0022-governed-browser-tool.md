# ADR-0022: The browser is a kernel-resident tool; egress is the boundary

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

Issue #503 (question-labeled) asks for an in-sandbox browser so the agent can
drive API-less sites. The triage reshaped it: split into mode-1 (in-sandbox,
untrusted/public web) and mode-2 (human-authorized local browser for
login-walled tools), and explicitly "do NOT make credential injection a v1
requirement." An outside design review added: the submit-only approval gate is
security theater (a click submits forms; `navigate` with query params is the
real exfil channel), so the control must be egress, not action approval; and
DOM-only (no vision) is a defensible v1.

## Decision

Ship **mode-1 only**, as a native tool:

1. **Session in the persistent Python kernel** (ADR-0008 reuse): a
   Playwright(sync) session with module-level handles that survive across tool
   calls, so it inherits the mandatory sandbox's egress posture, cgroups, and
   `--init` reaping with no new runtime. Kernel restart → a structured
   `BROWSER_SESSION_LOST` recoverable error, not a stack trace.
2. **Egress is the security boundary, not approvals.** No approval cards. The
   tool refuses in lockdown (clear per-call error — the hard seal is never
   weakened) and is meant to run under the allowlisted egress posture (#211),
   which structurally kills the navigate-exfil channel. This is honest: it
   claims a property it can actually deliver.
3. **DOM-only, no vision.** `read` returns page text + numbered interactive
   elements; screenshots go to the workspace for the human, never to the model
   — so there is no image-prompt-injection surface.
4. **No credentials in v1.** Login-walled sites + a human-authorized local
   browser operator are mode-2, deferred.
5. **Chromium is an optional client-bundle image layer**, not in the default
   image (Grype CVE gate + weight). A pinned reference block ships commented in
   the default Containerfile; the tool degrades to a clear `BROWSER_NOT_INSTALLED`
   error, and `FLEET_BROWSER_ENABLED` (default off) gates registration.

Absorbs #203 (Playwright MCP server): a native tool wins here because it
inherits the sandbox egress/resource discipline and the workspace-screenshot
path automatically — the exact security-critical wiring an MCP server would
have to re-implement.

## Alternatives rejected

- **Submit/click approval gate** — theater; the model routes around it via
  `click`, and it implies a boundary v1 can't deliver. Egress is the real one.
- **Vision loop in v1** — cost, latency, and an image-injection surface for no
  DOM-first capability gain.
- **A separate long-lived browser process in the sandbox** — buys OOM-blast
  isolation but costs a whole IPC/lifecycle layer; deferred (the `--init`
  reaper + session-lost handling cover v1's failure modes).
- **A Playwright MCP server (#203)** — would duplicate the egress/resource
  wiring that a native tool gets for free.
