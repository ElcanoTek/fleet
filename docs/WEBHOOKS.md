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
    notify_user: "brad@elcanotek.com"        # required: whose conversation this creates

  - slug: "slack-requests"
    description: "Triggered by a message in #fleet-requests"
    token_secret_env: SLACK_SIGNING_SECRET   # Slack v0 signing secret (mutually exclusive with hmac_secret_env)
    persona: "assistant"
    prompt_template: "{{.payload.event.user}} asks: {{.payload.event.text}}"
    notify_user: "brad@elcanotek.com"
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
| `202` | Accepted. Body is `{"conversation_id":"…"}`; the turn runs asynchronously. |
| `200` | Slack `url_verification` handshake echoed. |
| `400` | Body was not valid JSON. |
| `401` | Unknown slug **or** invalid/missing signature (deliberately indistinguishable — no slug enumeration). |
| `429` | Per-slug rate limit (`FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE`, default 10) or the per-owner concurrent-turn cap. |
| `500` | The `prompt_template` failed to render (detail is logged host-side, not returned). |
| `503` | The server is draining (graceful shutdown). |

The triggered conversation appears in `notify_user`'s conversation list the next
time they open Fleet. The metric `fleet_webhook_triggers_total{slug,result}`
counts requests (the `slug` label is populated only for configured triggers).

## Security summary

- **Auth is by signature, not session.** The endpoint is reachable without a
  Fleet login; the HMAC / Slack secret is the credential. No configured triggers
  ⇒ every call is rejected `401`.
- **The secret never enters the sandbox, the model, or the logs** — it is read
  host-side at request time, like an MCP connector credential.
- **The turn runs through the one governed core** (`agentcore.Run`): same policy,
  ceilings, audit, and mandatory sandbox as any chat turn.
- **A webhook acts as its configured `notify_user`** and cannot impersonate any
  other account.
