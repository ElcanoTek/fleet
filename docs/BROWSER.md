# Governed browser tool (#503)

An in-sandbox browser lets the agent operate websites that have no API — read a
page, click, type — on the **public/untrusted web**. It is a native tool
(`browser`), off by default, and DOM-first (v1). See ADR-0022 for the design
rationale.

## What it is (v1, "mode-1")

- **Session in the persistent Python kernel**: the browser is a Playwright
  session living in the same per-conversation IPython kernel as `run_python`
  (#213). Module-level handles survive across tool calls like REPL variables,
  so the browser inherits the mandatory sandbox's egress posture, resource
  cgroups, and `--init` process-reaping for free — no new runtime.
- **Actions**: `navigate {url}`, `read` (page text + a NUMBERED list of
  interactive elements), `click {ref}`, `type {ref, text}`, `screenshot` (PNG
  to the conversation workspace, rendered for the human — **not** fed to the
  model).
- **DOM-only**: no vision model in v1 — so no image-prompt-injection surface.
  Canvas/WebGL apps are out of scope.

## Security model

The boundary is **egress, not per-action approval**. A submit/click approval
card is theater (a click can submit a form; the real exfiltration channel is
`navigate` to a URL carrying stolen data in the query string), so v1 has no
approval cards and instead:

- **Refuses in lockdown** (`--network=none`): the tool returns a clear per-call
  `BROWSER_UNAVAILABLE` error. The hard seal is never weakened.
- **Is meant to run under allowlisted egress** (`FLEET_DEFAULT_NETWORK_MODE=allowlisted`,
  #211): `navigate` to a non-allowlisted host fails at the host CONNECT proxy
  regardless of what an injected page says. Under open egress the tool still
  works, but a deployment that cares about exfiltration should run allowlisted.
- **No credentials**: v1 has no login flows and injects no secrets. Login-walled
  sites and a human-authorized local browser operator are the documented
  mode-2 follow-on.

## Enabling it

1. Add the optional Chromium+Playwright layer to your **client-config**
   Containerfile (a pinned reference block is commented at the end of
   `config/default/sandbox/Containerfile`) and rebuild your sandbox image.
2. Set `FLEET_BROWSER_ENABLED=true`.
3. Run allowlisted: `FLEET_DEFAULT_NETWORK_MODE=allowlisted` with your task's
   target hosts in the bundle's `sandbox.network_allowlist`.

Without the image layer the tool returns a clear `BROWSER_NOT_INSTALLED` error.

## Honest scope (deferred)

- **Mode-2** (login-walled / paid tools via a human-authorized local browser
  operator with one-time control + instant stop) is not built.
- **No vision loop** — DOM-only; screenshots are for the human.
- **Interactive turns only** — scheduled/unattended browsing is a follow-on.
- **No CI e2e of real Chromium** — the snippet generation, gating, and result
  parsing are unit-tested; the real Chromium-in-rootless-Podman path is
  exercised per-deployment (a nightly gated job is the natural next step). This
  is called out because rootless Podman + Chromium has real friction
  (`/dev/shm`, user namespaces) the reference Containerfile documents.
