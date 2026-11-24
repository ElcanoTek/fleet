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
no slug or kind enumeration. The admin API key is never involved, and neither is
any other API key: the email endpoint ignores a `fleet_webhook_` key, so a key
sent without a valid signature gets the same 401.

### Each endpoint fires only its own kind

The kind check runs in both directions:

| Request | Answer |
| --- | --- |
| A webhook trigger's slug on `POST /triggers/email/{slug}` | The unknown-slug answer: **401** `{"detail":"unauthorized"}`, no run |
| An email trigger's slug on `POST /triggers/{slug}`, with a valid signature **or** a `fleet_webhook_` key that lists the slug | The unknown-slug answer: **401** `{"detail":"unauthorized"}`, no run |

Both take the handler's unknown-slug path: the lookup by slug and kind returns
no row, and the handler still computes one HMAC, against a per-process dummy
secret, before answering with the unknown-slug status and body. The answer
therefore does not show that the slug exists as a trigger of the other kind. An
email trigger runs only through the email endpoint, under every control below
and the connector opt-in. The generic endpoint applies none of them, so it never
fires an email trigger.

A `fleet_webhook_` key may list any well-formed slug: keys live in the key file,
not the trigger table, and a slug can be listed before its trigger exists or
reused after it is deleted, so key creation does not check kinds. The check is
made on every request instead: a key listing an email trigger's slug fires
nothing.

The rule holds by construction. The trigger lookup by slug always names the one
kind the endpoint fires (`storage.GetTriggerBySlug(ctx, kind, slug)`, with no
slug-only form), and each spawn function refuses a trigger of another kind
(`SpawnWebhookRun` fires webhook triggers only, `SpawnEmailRun` email triggers
only), so a future endpoint has to state which kind it fires.
`TestOpenAPITriggerRoutesFireOnlyTheirKind` (in `cmd/fleet`) sends a trigger of
every other kind in `models.TriggerKinds` to every trigger endpoint in the spec.

## Security controls (the email-kind policy)

Configured per trigger and enforced after authentication:

| Control | Behavior |
| --- | --- |
| **Approved senders** | The `From` address must match an allowlist entry — a full address (`alerts@corp.com`) or a bare domain (`corp.com`). No match → **403**. An email trigger MUST name at least one sender. |
| **DKIM** | `require_dkim` (default true) rejects anything whose provider-reported DKIM ≠ `pass` → **403**. |
| **SPF** | `require_spf` (default false — SPF breaks on legitimate forwarding) rejects SPF ≠ `pass` → **403**. |
| **Attachments** | `max_attachments` (default 0 = none) and `max_attachment_bytes` cap the declared attachments → **413**. |
| **Dedup / idempotency** | The `Message-ID` (or a content hash when absent) is the idempotency key. A duplicate delivery returns **200 `{"status":"duplicate"}`** and spawns no second run. A delivery that fails to start a run is not recorded, so the sender's retry runs it (see [Deliveries and retries](#deliveries-and-retries)). |
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
path is unchanged: it still inherits the template's MCP selection. Webhook
ingress is HMAC-authenticated against a per-trigger secret, so its trust model is
the operator holding that secret, not an arbitrary inbound sender. That rule
applies to webhook triggers only: the generic endpoint answers an email
trigger's slug as an unknown slug, so an email trigger never runs under it.)

### "None" is written down, not left blank (#979)

The opted-out spawn persists a **non-nil empty `credential_allowlist`** — the
explicit deny-all form. That distinction is the whole boundary, because the
allowlist has three states and only two of them are visible in a length check:

| Stored value | Meaning at the Gate-3 broker seam |
| --- | --- |
| `NULL` (nil) | **inherit global** — every seat the deployment has |
| `[]` (non-nil, empty) | **deny all** — no MCP call may be made |
| `[{server, account}, …]` | only the listed pairs |

Leaving the allowlist unset on the opted-out run therefore granted **every** seat
rather than none — the exact inverse of the documented rule, and invisible to a
test that only asserted the run "inherited nothing" (`len(...) == 0` is true for
both the top and middle rows). fleet now writes the deny down, and the tests
assert the negative: an `allow_event_triggers=false` spawn reaches **zero** seats.

Two consequences follow from that one value, both enforced in the scheduled
runner:

- **No connector is wired for the run at all** — not merely denied at call time.
  An empty `mcp_selection` means *the deployment default set* everywhere else
  (`taskMCPSelection(..., allWhenEmpty)`, the shared process-wide MCP client), so
  a deny-all run gets an empty selection and a dedicated empty client instead,
  and no `mcp_list_servers` / `mcp_load_servers` loader tools (calling
  `mcp_load_servers` spawns credentialed subprocesses as a side effect, which a
  run permitted to call nothing must not be able to do).
- **The task owner's hosted (OAuth) remote connections are skipped.** Those are
  attached from the run's *owner*, not from the task's own `mcp_selection`, so an
  operator auditing a connector-less template would see nothing while the run
  still reached whatever that human personally connected. A deny-all run does not
  open them, so no bearer is minted and no third party is dialed.

`mcp_selection` cannot carry the deny by itself: the column is coerced to `[]` on
write, so an "explicitly empty" selection is indistinguishable from an absent one.
The credential allowlist is the single load-bearing encoding of "none", and
`agentcore.CredentialAllowlist.DeniesAll()` is the one predicate that reads it.

The general trap, worth remembering wherever this codebase uses the same
sentinel: **nil-means-permissive is fine for a config default and wrong for a
security boundary.** Anything that has to express "denied" must express it as a
value, never as an absence.

## Tying the run to the event + reply-back

Every accepted email is recorded in a `trigger_events` row that links the inbound
message (sender, subject, `Message-ID`) to the spawned `run_id`, so a run is
always traceable to the email that started it.

When SMTP is configured for sending (`FLEET_SMTP_HOST` + `FLEET_SMTP_FROM`) and
the run **succeeds**, fleet replies to the original sender with the run's final
answer, threaded to the original message via `In-Reply-To`/`References`. Reply-back
is a no-op when SMTP isn't configured.

### Deliveries and retries

The `trigger_events` row is also the dedup record, and it is written in the
same database transaction that creates the run, already linked to it
(`storage.SpawnEmailRun`). The two commit together or not at all:

| What happens to a delivery | Answer | What the sender's retry of the same `Message-ID` gets |
| --- | --- | --- |
| The run is created | **202** `{"run_id": …}` | **200** `{"status":"duplicate"}`, no second run |
| The run cannot be created (a database error, a dropped connection, fleet stopping mid-request) | **500**, or no answer | Nothing was recorded, so the retry starts the run |
| The template's `prompt_template` fails to render | **400** | Nothing was recorded, so a retry after the template is fixed starts the run |
| The `Message-ID` holds U+0000, which the database cannot store, or is longer than an RFC 5322 line allows (998 bytes) | **400** `invalid message_id`, before any lookup | The same **400**; such an email never runs |

A delivery that fails to start a run is not recorded, so the sender's retry
runs it; the delivery is only "seen" once its run exists. Two deliveries of the
same `Message-ID` that arrive together start one run: the second waits for the
first's transaction, and is answered as a duplicate if the first created its
run, or creates the run itself if the first did not. Whether a failed delivery
is retried at all is up to the sending provider; fleet records nothing for it.
If the acknowledgement of the commit is lost (the connection drops as the
transaction commits), a delivery whose run was created can still be answered
**500**; the retry is then answered as a duplicate.

A redelivery of an email that already ran is answered as a duplicate before
the prompt is rendered, so it stays **200** `{"status":"duplicate"}` even if the
trigger's `prompt_template` was changed since to one that no longer renders (a
new email then gets the **400**).

`trigger_events` rows are kept: no job prunes them, and deleting a trigger
deletes its events (`ON DELETE CASCADE`). A redelivery is recognized for as long
as its row exists. Rows recorded before the event and its run were committed
together can name no run (`run_id` NULL, from a delivery whose render, spawn or
run link failed); a redelivery of one is still answered as a duplicate, and no
migration removes them.

## Managing email triggers (CLI)

```sh
# The template task must exist first (create it with trigger_type=webhook so the
# cron engine never runs it; set allow_event_triggers=true to let event runs use
# its connectors).
fleet sched trigger create \
  --task <template-task-uuid> \
  --slug weekly-deploy \
  --kind email \
  --approved-sender alerts@corp.com \
  --approved-sender corp.com \
  --require-dkim \
  --max-attachments 3 --max-attachment-bytes 1048576 \
  [--template prompt.tmpl]     # optional Go text/template over {{.From}} {{.Subject}} {{.Text}} {{.HTML}} {{.To}}

fleet sched trigger list         # shows id, kind, slug, task
fleet sched trigger rotate <id>  # rotate the HMAC secret
fleet sched trigger delete <id>
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
