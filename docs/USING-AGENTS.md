# Using other agents in fleet

fleet runs your turns through a selectable **runtime flavor**. Out of the box
that flavor is fleet's own agent loop — but fleet is, under the hood, an
[Agent Client Protocol](https://agentclientprotocol.com) (ACP) **client**, so it
can also drive *other* coding agents (Claude Code, Goose, …) as first-class,
sandboxed flavors you pick per chat or per scheduled task.

This guide is for the person wiring those agents into a deployment. It covers:

- [The flavor model](#the-flavor-model) — the three flavors and when to use each
- [Governance tiers](#governance-tiers-read-this) — what fleet does and does **not**
  guarantee for each, stated honestly (read this before you enable an external agent)
- [Adding an external agent flavor](#adding-an-external-agent-flavor) — the manifest
  entry, the provider image, and the model-cred env vars
- [Picking a flavor](#picking-a-flavor) — per chat and per scheduled task
- [The permission UI](#the-permission-ui) — how an external agent asks the human
- [Worked example: adding Goose](#worked-example-adding-goose)
- [Data-residency caveat](#data-residency-caveat) — the one thing you cannot wave away

Everything here is generic. The manifest snippets are written to be copied
straight into any `XYZ-config` client bundle's `manifest.yaml`.

---

## The flavor model

A **flavor** is one entry under `runtimes:` in your bundle's `manifest.yaml`.
Each has a `type`:

| `type`             | What runs                                                                 | Where tool calls execute              | Trust posture          |
| ------------------ | ------------------------------------------------------------------------- | ------------------------------------- | ---------------------- |
| `native-inprocess` | fleet's own loop, in the fleet process                                    | host (the per-turn sandbox)           | **Fully governed**     |
| `native-acp`       | fleet's own loop, wrapped as a sandboxed ACP agent (`fleet-native-agent`) | host (delegated back over ACP)        | **Fully governed**     |
| `acp` (external)   | a **third-party** agent (Claude Code / Goose) in its own sandbox          | inside the agent's sandbox (self-run) | **Containment** tier   |

- **`native-inprocess`** is the fast path and the parity oracle: today's
  in-process loop. Best for dev/test/trusted-local.
- **`native-acp`** is the recommended *sandboxed production* flavor. It is the
  exact same loop and the exact same governance as `native-inprocess` — the loop
  just runs inside a container and delegates every `bash`/`run_python` call back
  to the host, where fleet applies full policy/audit/notes/credential-injection.
  fleet is the parent; the agent cannot self-execute.
- **`acp` (external)** drives a *different vendor's* agent. That agent
  **self-executes inside its own locked sandbox** — it does not hand its tool
  calls back to fleet. This is a fundamentally different and weaker trust
  posture, and fleet treats it as such (see the next section).

All three are driven over the *same* ACP seam, so they are interchangeable from
the picker's point of view. They are **not** interchangeable from a governance
point of view, and fleet never pretends otherwise.

---

## Governance tiers (read this)

fleet stamps every external turn's tier into the session log (`governance:
delegated`) so the audit trail records, per turn, which posture it ran under.
There are two tiers. We document them honestly and never conflate them.

### Tier 1 — **Fully governed** (`native-inprocess`, `native-acp`)

fleet owns execution end to end:

- **Per-tool-call policy.** Every `bash`/`run_python`/MCP call passes through the
  host policy gate (cost ceilings, repeat detection, critical-tool approval
  staging, the email send gate, …).
- **Audit.** fleet records the real, executed tool calls — not a self-report.
- **Notes & context.** The notes wiki and per-conversation context are injected
  host-side.
- **MCP credentials.** Brokered host-side at the moment of delegation. They are
  **never** placed in any agent container.
- **Blast radius.** Tool calls run in fleet's own hardened per-turn sandbox.

### Tier 2 — **Containment** (`acp` external, `delegated_policy: true`)

The external agent self-executes. fleet cannot enforce per-tool policy on code
it does not run. So instead of *governing*, fleet **contains**:

- **`governance: delegated`** is stamped into the session log for the turn.
- **Audit is the agent's self-report.** fleet captures the agent's
  `session/update` stream (its narrated text, thoughts, and tool-call notices)
  as *observed / audited*, **not enforced**. If the agent under-reports what it
  did, fleet cannot know.
- **Locked sandbox.** The provider's agent runs `--read-only`, `--cap-drop=ALL`,
  `--security-opt=no-new-privileges`, with a **scratch-only** tmpfs workspace
  discarded on teardown.
- **Restricted egress.** `network: model_only` — the only network the agent is
  meant to reach is its model endpoint. (fleet stamps the intent and keeps the
  env scrubbed; enforcing `model_only` at the packet level is the host
  firewall's job — see the note in the worked example.)
- **Scrubbed env — no fleet secrets.** The agent container receives **only** the
  provider's own model key (the env var names you declare in `model_env`).
  fleet's secrets and your MCP credentials are never shipped to it.
- **Coordinated teardown.** fleet kills the whole process group + container; it
  never trusts `--rm` alone.
- **Permission gate.** Anything the agent flags as sensitive routes to a human
  (next section), **default-deny on timeout, no "approve all."**

> **The honest bottom line:** containment bounds what an external agent can *do
> on your host*. It does **not** stop a self-executing agent from sending what
> it *reads* (your workspace files) to its own model endpoint. See
> [Data-residency caveat](#data-residency-caveat). Use the external tier when the
> convenience of a specific vendor's agent is worth that trade-off — and only
> then.

---

## Adding an external agent flavor

Three things, all in your client bundle:

### 1. A `runtimes:` manifest entry

Add an entry of `type: acp` to `manifest.yaml`:

```yaml
runtimes:
  # … keep your native flavors …

  my-agent:                                   # the flavor key (shown in the picker)
    type: acp
    image: "ghcr.io/acme/my-agent@sha256:…"   # the provider sandbox image (DIGEST-PINNED)
    network: model_only                       # restrict egress to the model endpoint
    delegated_policy: true                    # the containment tier (governance: delegated)
    model_env: ["ACME_API_KEY"]               # env var NAME(S) carrying the provider's OWN model key
    args: ["acp"]                             # optional: extra args to the agent entrypoint
    display_name: "My Agent (external)"
    description: "Acme's agent over ACP. Self-executing; containment tier."
    beta: true
```

Field notes:

- **`image`** — pin by digest in production. This is the provider's agent baked
  into a container that speaks ACP over stdio (stdout = protocol, stderr =
  diagnostics; no PTY).
- **`network: model_only`** — the egress posture. fleet stamps the intent and
  keeps the env scrubbed regardless; pair it with a host egress policy that
  actually allows only the model endpoint.
- **`delegated_policy: true`** — marks the containment tier. Without it the
  flavor would not be treated as an external self-executing agent.
- **`model_env`** — the *names* of the env vars holding the provider's own model
  credential. At spawn, fleet reads these from **its own** environment and
  passes **only** them into the agent container. (So you put `ACME_API_KEY=…`
  in fleet's env file; fleet forwards it, and nothing else, into the sandbox.)
- **`args`** — appended to the container entrypoint. Goose, for instance, needs
  `["acp"]` to start its ACP server.

### 2. The provider sandbox image

Build (or pull) a container that runs the provider's agent as an ACP server over
stdio. The canonical patterns:

- **Claude Code** → the [`claude-agent-acp`
  bridge](https://github.com/zed-industries/claude-code-acp) (a.k.a.
  `@zed-industries/claude-code-acp`) wraps the Claude Code CLI as an ACP agent.
  Bake the bridge + the CLI into the image; the bridge reads `ANTHROPIC_API_KEY`.
- **Goose** → speaks ACP natively. The entrypoint is `goose acp` (hence
  `args: ["acp"]`). Bake the `goose` binary into the image.

Pin the base by digest and keep the image minimal — the agent self-executes
nothing fleet governs, so it only needs its own runtime.

> The generic `config/default` bundle ships **documented placeholder** entries
> for `claude-code` and `goose` (with `localhost/...` image refs) so the wiring
> is visible and testable. A real `XYZ-config` bundle overrides them with its own
> digest-pinned images and its own model creds.

### 3. The model-cred env vars

Put the provider's model key in fleet's env file (the same env file that holds
the rest of fleet's config), using the variable name(s) you listed in
`model_env`:

```sh
# fleet's env file
ACME_API_KEY=sk-acme-…
```

That key is the **only** secret that ever reaches the external agent's
container.

---

## Picking a flavor

### Per chat

The chat composer shows a **runtime picker** whenever the bundle defines more
than one flavor. Click it, choose a flavor; the choice is stored on the
conversation (`POST /conversations/{id}/runtime`) and applies to that
conversation's next turn. An empty/cleared choice falls back to the bundle's
`default:` flavor.

### Per scheduled task

The orchestrator selects a flavor per task the same way (the task's runtime
field). A bundle that wants to keep scheduled work fully governed simply does
not offer external flavors to the scheduler — external scheduled runs are gated
behind a per-client flag and default off.

A flavor name that no longer exists in the catalog falls back to the default at
run time, so a stale picker value can never wedge a turn.

---

## The permission UI

When an external agent decides an action is sensitive, it sends
`session/request_permission` over ACP. fleet routes that to **a human**:

1. fleet emits a `permission.requested` event over SSE. The agent's turn
   **blocks** server-side, waiting.
2. The chat UI renders an inline **allow / deny** card showing what the agent
   wants to do — the title, the affected file paths, and the agent's offered
   options.
3. The user clicks **Allow** (picking one of the agent's allow-shaped options)
   or **Deny**. The decision POSTs to
   `/conversations/{id}/permissions/{requestId}`, the agent's turn unblocks, and
   it proceeds or skips accordingly.

Two safety properties are non-negotiable and enforced server-side:

- **Default-deny on timeout.** If no human answers within the window, the
  request is **denied**, never allowed. No human, no allow.
- **No "approve all."** Every request is decided on its own merits. fleet does
  not surface a one-click blanket approval, even if the agent offers an
  `allow_always` option (it is treated as a one-time allow).

A misconfigured external flavor with no permission broker **fails closed** —
every request is denied.

---

## Worked example: adding Goose

[Goose](https://github.com/block/goose) speaks ACP natively, which makes it a
clean example.

**1. Build the image.** A minimal container with the `goose` binary, entrypoint
left as the binary so `args: ["acp"]` starts the ACP server:

```Dockerfile
FROM debian:stable-slim@sha256:…           # pin by digest
RUN install-goose.sh                       # however you obtain the binary
ENTRYPOINT ["goose"]
```

Build and (optionally) push it:

```sh
podman build -t ghcr.io/acme/fleet-goose:latest -f Containerfile.goose .
```

**2. Add the flavor** to your bundle's `manifest.yaml`:

```yaml
runtimes:
  goose:
    type: acp
    image: "ghcr.io/acme/fleet-goose@sha256:…"   # the digest you just built
    network: model_only
    delegated_policy: true
    model_env: ["OPENROUTER_API_KEY"]            # Goose-on-OpenRouter; use your provider's key name
    args: ["acp"]
    display_name: "Goose (external)"
    description: "Block's Goose via native ACP. Self-executing; containment tier."
    beta: true
```

**3. Supply the model key** in fleet's env file:

```sh
OPENROUTER_API_KEY=sk-or-…
```

**4. Restrict egress.** Pair `network: model_only` with a host firewall/egress
rule that allows only your model endpoint from the Goose container's network.
fleet keeps the container env scrubbed regardless, but the packet-level
restriction is the host's responsibility — `network: model_only` is the
*declaration*, your firewall is the *enforcement*.

**5. Pick it.** Reload fleet (so it re-reads the bundle), open a chat, choose
**Goose (external)** in the runtime picker, and send a turn. Watch for the
permission card the first time Goose tries something sensitive.

That's it. Claude Code is identical except you bake the
[`claude-agent-acp`](https://github.com/zed-industries/claude-code-acp) bridge
into the image and set `model_env: ["ANTHROPIC_API_KEY"]`.

---

## Data-residency caveat

This is the one thing you must not wave away when enabling an external flavor.

An external agent runs your turn against **its own** model endpoint. To do useful
work it reads your workspace — the files, data, and context of the task — and
those reads go into the prompts it sends to its provider. **Containment cannot
prevent this**: it bounds what the agent can *do on your host*, not what it can
*transmit about what it reads*.

So: an external flavor sends your workspace contents to that vendor's model
endpoint. If your data has residency, confidentiality, or contractual
constraints, only enable external flavors whose provider you are contractually
comfortable sending that data to — and prefer the **fully-governed** native
flavors (`native-inprocess` / `native-acp`) for anything sensitive. fleet stamps
this caveat into the session log alongside every external turn so the record is
unambiguous.

---

## How it works (for the curious)

- fleet is an ACP **client** (`internal/acpruntime`). For an external flavor it
  spawns the provider's image via `podman run -i`, drives
  `Initialize → NewSession → Prompt`, and implements `acp.Client`.
- The external client (`internal/acpruntime/external.go`) **forwards** the
  agent's `session/update` stream onto fleet's `Observer` (→ SSE / session log) —
  that self-report is the containment-tier audit — and routes
  `session/request_permission` to the `PermissionBroker` (the human, default-deny).
- The native flavors instead delegate `bash`/`run_python` back to the host over
  the `_fleet/*` ACP extension methods, where fleet's real `agentcore.Executor`
  runs them under full policy. That is what makes them *governed* rather than
  merely *contained*.

The deterministic test provider for the external path is the credential-free ACP
example agent in `cmd/acp-example-agent` (a turn streams, a permission request is
handled) — proving the generic external path end-to-end without any provider
credentials. Claude Code and Goose are wired the same way.
