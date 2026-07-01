# ADR-0016: Webhook-triggered conversations

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

fleet already lets an external system fire a **scheduled task** over an inbound
webhook: `POST /triggers/{slug}` on the orchestrator (ADR-less, issue #177/#190)
authenticates a caller by a per-trigger HMAC-SHA256 secret (or a slug-scoped
`fleet_webhook_` API key) and spawns a one-shot task from a template. There was
**no** equivalent for an *interactive* conversation: "when this GitHub PR opens
(or this Slack message arrives), start a Fleet chat and let the agent handle it,
so its transcript lands in a real user's conversation history."

Issue #268 asks for that. It requires a **new externally-reachable HTTP
endpoint** on the chat server — the surface that has, until now, been reachable
only through the trusted Next.js proxy after session verification. An endpoint
that runs the agent from an unauthenticated caller's payload is a meaningful
addition to the threat model, so per the ADR convention it is recorded here.

## Decision

Add `POST /webhooks/{slug}` to the chat server. A caller that presents a valid
signature starts a fresh conversation under a manifest-configured owner
(`notify_user`), seeded with a prompt rendered from the trigger's
`prompt_template` against the request payload. Triggers are declared in the
client-config bundle manifest (`webhook_triggers:`), the same operator-authored,
trusted surface that already declares `mcp_servers` and `http_tools`.

### The run is the one governed core

The handler is an **inbound I/O adapter**, not a second agent loop. After
authenticating and rendering the prompt it calls the *same*
`runTurnAsync → agent.RunTurn → agentcore.Run` path `POST /chat` uses — it
differs only in that it never attaches an SSE stream (it returns
`202 {"conversation_id": …}` and the turn runs fire-and-forget). This preserves
the **"governance is one core"** invariant: policy, the cost/token/iteration
ceilings, the audit ledger, and the mandatory sandbox all apply unchanged.

### Authentication: signature, not session

The endpoint is registered **outside** the `auth(member(mutate(…)))` middleware
chain — like `/healthz` and `/shared/` — because GitHub, Slack, and CI cannot
present a Fleet session token. Authenticity is proven instead by:

- **GitHub-style HMAC** (`hmac_secret_env` + `hmac_header`, default
  `X-Hub-Signature-256`): constant-time HMAC-SHA256 over the raw body.
- **Slack v0 signing secret** (`token_secret_env`): HMAC over
  `v0:{timestamp}:{body}`, with the timestamp constrained to a ±5-minute replay
  window.

Both verifiers, and the timing-equalization dummy secret, live in **one place** —
`internal/webhooks` — shared with the orchestrator trigger. There is deliberately
a single implementation of this security-critical primitive so a subtle mistake
(a non-constant-time compare, an unbounded replay window) cannot exist in one
endpoint but not the other. The secret **value** is read host-side from the
process env at request time (its name is registered via `EnvVarNames →
RegisterAllowedEnvVars`, so it flows from `.env` exactly like an MCP connector
credential) and never enters the sandbox, the model context, or the logs.

### No slug enumeration

An unknown slug and a bad signature return an **identical, timing-equalized
`401`** (never a `404` — that would be an enumeration oracle). The miss path
still performs one HMAC-SHA256 against a per-process random dummy secret before
failing closed, so response timing does not distinguish a configured slug with a
bad signature from a slug that does not exist. This mirrors the orchestrator
trigger's shipped behavior.

### The payload is untrusted model input

This is the security posture an operator must understand:

- A trigger **definition** (slug, secret env, persona, template, owner) is
  operator-authored and trusted.
- The inbound **payload** is attacker-controllable. It is exposed to the template
  only as **data** (`{{.payload…}}` / `{{.raw}}`), never as the template text,
  and the rendered result is **untrusted input to the model** — a prompt-injection
  surface, the same as any untrusted content a user pastes into chat.

Containment is layered, not a claim that injection is impossible:

1. **The mandatory sandbox.** Every tool call the triggered agent makes runs in
   the rootless-Podman sandbox under host policy — the same containment as any
   turn. The webhook opens no new tool-execution path. It also honors the
   server-wide `CHAT_LOCKDOWN_ONLY` seal exactly like `POST /chat`: on a
   lockdown-only box the triggered turn runs in the same `--network=none`
   sandbox as every human turn (a webhook, being an external caller, cannot opt a
   conversation *out* of the global seal).
2. **The operator-chosen persona** bounds the agent's role and (via #294 persona
   tool policies) can narrow its tool roster.
3. **The per-turn cost / token / iteration ceilings** bound the blast radius of a
   runaway or adversarial payload.
4. **Rate limiting.** A per-slug `10 req/min` cap (`FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE`)
   throttles authenticated turn-spawns, plus the per-owner concurrent-turn cap.
5. **A 1 MiB body cap** and the outer IP filter / body-limit middleware.

The operator is responsible for scoping the persona and template so an
adversarial payload cannot direct the agent somewhere it should not go. The docs
say this plainly (`docs/WEBHOOKS.md`).

### Identity is the configured owner

The turn runs as `notify_user` — a real, operator-configured account — so it uses
that account's memories, MCP opt-ins, and conversation history, and the
transcript appears in their list. A webhook **cannot** impersonate an arbitrary
user; it can only act as the owner the manifest names. `notify_user` is required
and validated at load.

## Consequences

- A new externally-reachable endpoint exists on the chat server. It is
  unauthenticated *by session* but authenticated *by signature*; a deployment
  that does not declare any `webhook_triggers` has the endpoint reject every call
  (fail-closed `401`), so the default posture is unchanged.
- Prompt-injection risk is **accepted and documented**, bounded by the sandbox +
  persona + ceilings + rate limits above. It is not eliminated — that is inherent
  to letting an external event drive an agent, and is the operator's to scope.
- The webhook verification primitive is now shared (`internal/webhooks`); the
  orchestrator trigger was refactored onto it with byte-identical behavior.

### Honest scope (not in this change)

- **No per-trigger cost ceiling.** `TaskCreate`/the chat turn carry no per-run
  dollar field today (the ceiling is the process-wide `FLEET_MAX_COST_USD`), so a
  `max_cost_usd:` manifest knob would be a control with no sink. It is omitted
  rather than faked; the run is bounded by the global ceiling.
- **No slug-scoped `fleet_webhook_` API-key alternative** (the orchestrator has
  one). The chat server has no API-key manager — that is a sched concept — so the
  chat webhook authenticates by HMAC / Slack signature only.
- **No admin/web UI** for webhook triggers; they are configured in the manifest.
- **JSON payloads only** (GitHub webhooks + the Slack Events API). Slack
  slash-command form-encoding is not decoded.
- **No runtime `notify_user` membership check.** The manifest is trusted; a typo'd
  owner simply creates a conversation that surfaces if/when that account logs in.
