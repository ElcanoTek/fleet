# Webhook-triggered conversations (#268)

An external system — GitHub, Slack, CI, a Zapier hook — can start a Fleet
conversation over a signed HTTP webhook: `POST /webhooks/{slug}`. The agent
handles the event and its transcript lands in a configured user's conversation
history, ready to continue interactively.

This complements the orchestrator's `POST /triggers/{slug}`, which spawns a
scheduled **task**; webhook-triggered *conversations* are the interactive
complement. See [`docs/adr/0016-webhook-triggered-conversations.md`](adr/0016-webhook-triggered-conversations.md)
for the security model.

## Configure a trigger

Declare triggers in your client-config bundle's `manifest.yaml`:

```yaml
webhook_triggers:
  - slug: "github-pr-review"                 # URL path segment: ^[a-z0-9][a-z0-9_-]{0,127}$
    description: "Triggered when a GitHub PR is opened"
    hmac_secret_env: GITHUB_WEBHOOK_SECRET   # env var holding the shared secret
    hmac_header: "X-Hub-Signature-256"       # optional; this is the default
    persona: "code-reviewer"
    model: "anthropic/claude-sonnet-4-6"     # optional; falls back to the server default
    prompt_template: |
      A pull request was opened: {{.payload.pull_request.title}}
      URL: {{.payload.pull_request.html_url}}
      Please review it for correctness, security, and code quality.
    notify_user: "admin@example.com"         # required: whose conversation this creates

  - slug: "slack-requests"
    description: "Triggered by a message in #fleet-requests"
    token_secret_env: SLACK_SIGNING_SECRET   # Slack v0 signing secret (mutually exclusive with hmac_secret_env)
    persona: "assistant"
    prompt_template: "{{.payload.event.user}} asks: {{.payload.event.text}}"
    notify_user: "admin@example.com"
```

Set the secret in your `.env` (its name is auto-allowlisted from the manifest):

```
GITHUB_WEBHOOK_SECRET=…
SLACK_SIGNING_SECRET=…
```

### Fields

| Field | Required | Notes |
|-------|----------|-------|
| `slug` | yes | URL-safe, unique within the manifest. Forms the path segment. |
| `notify_user` | yes | Email whose conversation store the trigger writes to. Determines whose memories/MCP opt-ins/history apply. |
| `hmac_secret_env` **or** `token_secret_env` | one | HMAC (GitHub-style) *or* Slack v0 signing secret — set exactly one. |
| `hmac_header` | no | Signature header for HMAC triggers. Default `X-Hub-Signature-256`. |
| `persona` | no | Persona for the conversation. Defaults to the server persona default. |
| `model` | no | OpenRouter model slug. Defaults to the server default. |
| `prompt_template` | no | Go `text/template`. `{{.payload}}` is the decoded JSON body; `{{.raw}}` the raw string. Empty → the raw body becomes the prompt. |

## The prompt template

`prompt_template` is a Go [`text/template`](https://pkg.go.dev/text/template).
The inbound JSON body is exposed as `.payload` (a decoded map) and `.raw` (the
raw string). Missing keys render as empty (`missingkey=zero`), so a template
never fails on an absent field.

> **The payload is untrusted.** It is attacker-controllable and reaches the model
> as data. Treat a webhook prompt like any untrusted content: scope the `persona`
> and template so an adversarial payload cannot steer the agent somewhere it
> should not go. The mandatory sandbox, the per-turn cost/iteration ceilings, and
> the rate limits below bound the blast radius — they do not make injection
> impossible.

## Calling the endpoint

### GitHub

Point a repository webhook at `https://your-fleet/webhooks/github-pr-review`,
content type `application/json`, secret = `GITHUB_WEBHOOK_SECRET`. GitHub signs
the body and sends `X-Hub-Signature-256`.

### Slack

Set your Slack app's Request URL to `https://your-fleet/webhooks/slack-requests`.
Fleet answers the `url_verification` handshake automatically (it echoes the
`challenge`). Slack signs every request with `X-Slack-Signature` +
`X-Slack-Request-Timestamp`; requests older than 5 minutes are rejected.

### Manual (HMAC)

```sh
BODY='{"pull_request":{"title":"Fix the widget","html_url":"https://…"}}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$GITHUB_WEBHOOK_SECRET" | awk '{print $2}')"
curl -sS -X POST https://your-fleet/webhooks/github-pr-review \
  -H "Content-Type: application/json" \
  -H "X-Hub-Signature-256: $SIG" \
  -d "$BODY"
# → 202 {"conversation_id":"…"}
```

## Responses

| Status | Meaning |
|--------|---------|
| `202` | Accepted: the turn's input is saved. Body is `{"conversation_id":"…"}`; the turn itself runs asynchronously. |
| `200` | Slack `url_verification` handshake echoed, **or** a repeat of an already-accepted delivery: body `{"conversation_id":"…","duplicate":true}` names the conversation the first delivery started, and nothing new runs. |
| `409` | A repeat arrived while the first request for the same delivery had not yet saved its turn's input (or this request lost its claim to such a repeat). Retry later. |
| `400` | Body was not valid JSON. |
| `401` | Unknown slug **or** invalid/missing signature (deliberately indistinguishable — no slug enumeration). |
| `429` | Per-slug rate limit (`FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE`, default 10) or the per-owner concurrent-turn cap. |
| `500` | The `prompt_template` failed to render (detail is logged host-side, not returned), or the turn's input could not be saved within 10 seconds; nothing was accepted, so retry. |
| `503` | The server is draining (graceful shutdown). |

The triggered conversation appears in `notify_user`'s conversation list the next
time they open Fleet. The metric `fleet_webhook_triggers_total{slug,result}`
counts requests (the `slug` label is populated only for configured triggers;
`result` is `ok`, `duplicate`, `rejected`, `throttled`, `error` or `conflict`).

## Delivery idempotency

Senders deliver at least once: Slack retries an event it did not see answered
in time (with `X-Slack-Retry-Num`), and GitHub's **Redeliver** re-sends a
delivery under the same `X-GitHub-Delivery` id. Each trigger therefore remembers
the deliveries it accepted for **24 hours** and starts at most one conversation
per delivery. A delivery is identified, per slug, by the first of:

1. the `X-GitHub-Delivery` header;
2. for a Slack trigger, the payload's `event_id` (the same on every retry of
   an Events API event, whose timestamp and signature change per attempt);
3. otherwise the SHA-256 of the signature header plus the body — the same
   signed request sent twice is one delivery.

A repeat of an accepted delivery is answered `200` with the original
`conversation_id` (see Responses) and does not count against the per-slug rate
limit. The conversation is created and recorded against the delivery in one
database transaction; the delivery counts as accepted only when the turn's
input (the rendered prompt) is saved as its first message, in the same
transaction that marks the delivery complete. **The endpoint answers `202` only
after that** — it waits for the input to be saved, for at most 10 seconds — so a
`2xx` always means the event is durably taken. Until then a repeat is answered
`409`. If the input cannot be saved in time (or the turn fails before saving
it), the turn is stopped, the delivery is given back, the conversation created
for it is discarded while it is still empty, and the answer is `500`, so the
sender retries. A delivery that was not accepted for any other reason
(throttled, refused) is not remembered either. Each claim carries a random
token: a request still unfinished after a minute loses its claim to a repeat,
and from then on can neither record nor complete anything. A repeat that takes
a delivery over always starts afresh in a new conversation, under the server's
current settings (lockdown, model); the earlier request's conversation is
deleted if it is still empty and otherwise left as it is.

Because of the wait, the first answer can take as long as the turn takes to
get ready to start (seconds, usually). A sender with a short timeout (Slack
expects an answer within 3 seconds) may retry meanwhile; that retry is answered
`409` while the first request is still working, and `200` with the
conversation once it has finished.

Two different events with byte-identical bodies and no
delivery id (case 3) within 24 hours are treated as one; senders that need
otherwise should send a distinguishing field or header.

The records live in the chat database (`webhook_deliveries`, migration 098), so
they survive a restart inside a sender's retry window; the regular retention
sweep removes records older than 24 hours.

## Security summary

- **Auth is by signature, not session.** The endpoint is reachable without a
  Fleet login; the HMAC / Slack secret is the credential. No configured triggers
  ⇒ every call is rejected `401`.
- **The secret never enters the sandbox, the model, or the logs** — it is read
  host-side at request time, like an MCP connector credential.
- **The turn runs through the one governed core** (`agentcore.Run`): same policy,
  ceilings, audit, and mandatory sandbox as any chat turn. It honors
  `FLEET_LOCKDOWN_ONLY` too — on a lockdown-only server the triggered conversation
  runs network-sealed like every human turn.
- **A webhook acts as its configured `notify_user`** and cannot impersonate any
  other account.
