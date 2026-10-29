# Task notifications (email + webhook) & admin management

fleet notifies about scheduled-task outcomes over three channels, all fanned
out by one pipeline (`internal/notify`): **email** (SMTP), a **signed
outbound webhook** ([WEBHOOK-SIGNING.md](WEBHOOK-SIGNING.md)), and per-user
**browser Web Push** ([PUSH-NOTIFICATIONS.md](PUSH-NOTIFICATIONS.md)). Email
and webhook are deployment-wide; push is per-user. Default OFF — with nothing
configured, nothing fires.

## Configuring from the web UI (recommended)

Settings → Admin → **Notifications** lets an admin configure the email and
webhook channels at runtime:

- **Notify on**: which statuses fire (`success` / `failure` / `blocked` /
  `progress`; none selected = all terminal statuses). One filter, shared by
  every channel. **`failure` includes `blocked`**: a deployment that listens
  only for failures still hears about a blocked run (see
  [Blocked runs](#blocked-runs) below).
- **Email (SMTP)**: host, port, optional username/password, from address,
  recipient list.
- **Webhook**: URL, method (POST/PUT/PATCH), optional Go `text/template` body
  override, optional HMAC signing secret.
- **Send test** per channel: one real delivery attempt of a synthetic event
  using the *saved* config, reporting a key-free ok/detail/latency result.
- **Daily owner digest**: off by default; when on, one email a day at the
  chosen UTC hour (13:00 by default) to each task owner listing their
  scheduled tasks that need attention. It uses the SMTP host and From address
  above but goes to the owner, not the recipient list, and the *Notify on*
  filter does not apply to it. See [OWNER-DIGEST.md](OWNER-DIGEST.md).

Saving applies **live** — the next task completion (and budget alert, and
email reply-back) uses the new config, no restart. The saved config replaces
the env config **wholesale** (no field-level merging between the two sources);
**Use env config** deletes it and the env vars serve again.

Secrets (the SMTP password and the webhook signing secret) are **write-only**:
sealed with the store cipher (`FLEET_MCP_OAUTH_ENCRYPTION_KEY`, AAD-bound,
same treatment as LLM provider keys), never returned by any endpoint, and
decrypted host-side only to build the live notifier or run a test. Storing a
secret without the cipher key configured fails closed with guidance. Secrets
never enter the sandbox, the model context, or logs.

If the encryption key is rotated or lost, the saved secrets become
undecryptable: the env-derived config keeps serving, and the panel stays up
showing a warning with the two recovery paths — re-enter the secrets and
save, or revert to env config. (A degraded state never takes the panel down;
that would strand the admin with no UI path to fix it.)

## Configuring from the env file (the deployment default)

| Env var | Default | Meaning |
| --- | --- | --- |
| `FLEET_NOTIFY_ON` | empty = all terminal | CSV of `success`/`failure`/`blocked`/`progress`/`always` (`failure` also fires for `blocked`) |
| `FLEET_SMTP_HOST` | — | email off without it |
| `FLEET_SMTP_PORT` | `587` | |
| `FLEET_SMTP_USERNAME` / `FLEET_SMTP_PASSWORD` | — | optional SMTP auth (password is a secret) |
| `FLEET_SMTP_FROM` | — | also enables email reply-back (#511) with a host |
| `FLEET_NOTIFY_EMAIL_TO` | — | CSV recipients; email off without it |
| `FLEET_WEBHOOK_URL` | — | webhook off without it |
| `FLEET_WEBHOOK_METHOD` | `POST` | |
| `FLEET_WEBHOOK_BODY_TEMPLATE` | built-in JSON | Go `text/template` over the event |
| `FLEET_WEBHOOK_SECRET` | — | outbound HMAC signing key (secret) — see [WEBHOOK-SIGNING.md](WEBHOOK-SIGNING.md) |
| `FLEET_NOTIFY_TIMEOUT` / `FLEET_NOTIFY_RETRIES` | 10s / 2 | timing knobs — env-only by design |
| `FLEET_PUBLIC_URL` | — | absolute base for the log link (`<base>/orchestrator/tasks/<id>`, which opens that task's record in the Operations Center) — env-only |

Precedence: **admin row (when saved) > env vars**. Timing knobs and the
public URL base stay env-derived even under an admin row.

## Event statuses

| Status | Raised when | Notes |
| --- | --- | --- |
| `success` | a task reached `success` | not for a blocked run, which is `blocked` |
| `failure` | a task errored, dead-lettered or was interrupted | a dead-letter that stopped a recurring schedule carries `Schedule stopped: <reason>` as its message |
| `blocked` | a task reached `success` but its completion clause recorded a blocked outcome (`run_outcome = 'blocked'`, [CONDITIONAL-TASK-COMPLETION.md](CONDITIONAL-TASK-COMPLETION.md#a-blocked-outcome-is-visible-completionblocked_when)) | new in 2026-10 ([ADR-0092](adr/0092-blocked-is-a-notification-status.md)); the message is the recorded reason |
| `progress` | not terminal: an ask-pause, the agent's `notify` tool, or a budget soft alert | |

### Blocked runs

A blocked run finished, but recorded that it did not do its job (for a Pages
refresh: its source was unreachable, so the page was not updated). The task's
status stays `success`; the notification says `blocked`:

- **Email:** subject `Fleet task blocked: <name>`; the body starts with
  `Blocked: <name>` and gives the recorded reason as `Reason: …`.
- **Webhook (default body):** `"status":"blocked"` plus one extra key,
  `"reason"`, carrying the recorded reason. No other status gains a key, so
  the payload change is additive. A custom template reads the reason as
  `{{.Message}}` and can test `{{if eq .Status "blocked"}}`.
- **Web Push:** `⚠ Blocked: <name> (<duration>)`, without the reason (a push
  never carries the message).
- The run that makes three blocked occurrences in a row of a recurring
  schedule says so: its message starts `Blocked 3 runs in a row — …`. (Until
  2026-10 that run was sent as a `failure` and the others as `success`.)

**Filtering:** `failure` in the *Notify on* list (or `FLEET_NOTIFY_ON`) also
fires for `blocked`; `blocked` can be listed on its own to get blocked runs
without real failures; `success` never includes it. Webhook receivers that
switch on `status` should handle the new `blocked` value.

> Do not confuse `FLEET_WEBHOOK_SECRET` (OUTBOUND signing, this page) with the
> per-trigger secrets of INBOUND webhook triggers
> ([EVENT-TRIGGERS.md](EVENT-TRIGGERS.md)) — the inbound side is separate and
> deliberately not runtime-editable.

## Honest scope / deferred

- The admin panel covers email + webhook. Web Push (VAPID keys) stays
  env-configured — the keys are deployment identity material bound at boot.
- One deployment-wide config — no per-user or per-task notification routing
  (a task's owner does get per-user push, which is its own system, and the
  daily owner digest when an admin turns it on). The digest has no env var:
  under the env config it is off.
- The test button exercises the **saved** config, not unsaved form edits (the
  UI says so next to the result).
