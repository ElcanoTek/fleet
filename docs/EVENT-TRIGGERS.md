# Event-driven triggers — email ingress (#511)

Beyond cron and the generic webhook trigger (#177), a task can wake on an
**inbound email**. This is the v1 of "event-driven triggers 2.0"; it deliberately
reuses the proven webhook-trigger machinery rather than forking a second ingress
path. See [`adr/0027-event-driven-triggers.md`](adr/0027-event-driven-triggers.md)
for the design rationale and security posture.

## How it works

Fleet does **not** run an SMTP server. Inbound email arrives the way Manus'
"Mail Manus" flow works: you point an email provider's **inbound-parse webhook**
(Postmark / Mailgun / SendGrid inbound parse, or a tiny forwarder of your own) at
a per-task address, and the provider POSTs a normalized JSON representation of
each message to:

```
POST /triggers/email/{slug}
```

The provider performs DKIM/SPF verification and includes the *result* in the
payload; fleet consumes that verdict (it cannot re-verify DKIM without the raw
signed message and DNS). Each accepted email spawns one governed run cloned from
the trigger's template task — the same one-shot run the webhook path spawns, going
through the same `agentcore.Run` governed loop.

### Normalized payload

```json
{
  "message_id": "<abc@mail>",
  "from": "Alerts <alerts@corp.com>",
  "to": "task-weekly@inbound.example.com",
  "subject": "Deploy request",
  "text": "Please deploy v2 to staging.",
  "html": "<p>…</p>",
  "spf": "pass",
  "dkim": "pass",
  "attachments": [{ "filename": "log.txt", "size": 1024, "content_type": "text/plain" }]
}
```

Attachment **metadata** only is used in v1 (fleet does not fetch attachment
bytes); the policy's count/size limits are enforced against it.

## Authentication

Identical to the webhook trigger (#177): the provider signs the raw body with the
per-trigger HMAC-SHA256 secret and sends it as `X-Hub-Signature-256` (or
`X-Fleet-Signature-256`). An unknown slug, a webhook-kind slug hit on the email
endpoint, or a bad signature all return an **identical, timing-equalized 401** —
no slug or kind enumeration. The admin API key is never involved.

## Security controls (the email-kind policy)

Configured per trigger and enforced after authentication:

| Control | Behavior |
| --- | --- |
| **Approved senders** | The `From` address must match an allowlist entry — a full address (`alerts@corp.com`) or a bare domain (`corp.com`). No match → **403**. An email trigger MUST name at least one sender. |
| **DKIM** | `require_dkim` (default true) rejects anything whose provider-reported DKIM ≠ `pass` → **403**. |
| **SPF** | `require_spf` (default false — SPF breaks on legitimate forwarding) rejects SPF ≠ `pass` → **403**. |
| **Attachments** | `max_attachments` (default 0 = none) and `max_attachment_bytes` cap the declared attachments → **413**. |
| **Dedup / idempotency** | The `Message-ID` (or a content hash when absent) is the idempotency key. A duplicate delivery returns **200 `{"status":"duplicate"}`** and spawns no second run. |
| **Rate limit** | The same per-slug limiter as the webhook route (`FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE`). |

A **nil / unset** policy is treated as the most restrictive posture (no approved
senders ⇒ reject all, DKIM required, no attachments), so a misconfigured trigger
fails closed.

## The security default: connectors are opt-in

An event-spawned run inherits the template task's **write-capable connectors only
when the template task set `allow_event_triggers: true`**. Off (the default), an
email-spawned run gets **native tools only** — no MCP selection, no credential
allowlist — so an untrusted inbound email can never auto-escalate through a
connector. This is the hard rule from the issue. (The pre-existing #177 webhook
path is unchanged: it still inherits the template's MCP selection.)

## Tying the run to the event + reply-back

Every accepted email is recorded in a `trigger_events` row that links the inbound
message (sender, subject, `Message-ID`) to the spawned `run_id`, so a run is
always traceable to the email that started it.

When SMTP is configured for sending (`FLEET_SMTP_HOST` + `FLEET_SMTP_FROM`) and
the run **succeeds**, fleet replies to the original sender with the run's final
answer, threaded to the original message via `In-Reply-To`/`References`. Reply-back
is a no-op when SMTP isn't configured.

## Managing email triggers (CLI)

```sh
# The template task must exist first (create it with trigger_type=webhook so the
# cron engine never runs it; set allow_event_triggers=true to let event runs use
# its connectors).
fleet-admin sched trigger create \
  --task <template-task-uuid> \
  --slug weekly-deploy \
  --kind email \
  --approved-sender alerts@corp.com \
  --approved-sender corp.com \
  --require-dkim \
  --max-attachments 3 --max-attachment-bytes 1048576 \
  [--template prompt.tmpl]     # optional Go text/template over {{.From}} {{.Subject}} {{.Text}} {{.HTML}} {{.To}}

fleet-admin sched trigger list         # shows id, kind, slug, task
fleet-admin sched trigger rotate <id>  # rotate the HMAC secret
fleet-admin sched trigger delete <id>
```

The rendered prompt is what the spawned run receives. With no `--template`, a
sensible default prompt is built from the email (from + subject + body), so an
email trigger is useful out of the box.

## Honest scope / deferred

- **Source-change ingress** (poll/subscribe connected sources → run) is the
  issue's v2 and is **not** in this PR — the maintainer's sequencing is email
  ingress first, then a trigger-management UI, then source-change. It will reuse
  this same `task_triggers` table + the `allow_event_triggers` opt-in.
- **Trigger-management web UI** is deferred — management is CLI + API for now
  (as webhook triggers #177 shipped).
- **Reply "needs-input"** (a mid-run question back to the sender) ties into the
  ask/notify pause (#510) and is a follow-on; v1 replies with the result on
  success only.
- **Per-connector / per-trigger-source granularity** — the opt-in is currently
  all-or-nothing (`allow_event_triggers`); a per-connector matrix is a follow-on.
- Fleet hosts no SMTP server and does not ingest attachment content (metadata
  only); JSON payloads only.
