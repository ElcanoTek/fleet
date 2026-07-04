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

- **Notify on**: which statuses fire (`success` / `failure` / `progress`;
  none selected = all terminal statuses). One filter, shared by every channel.
- **Email (SMTP)**: host, port, optional username/password, from address,
  recipient list.
- **Webhook**: URL, method (POST/PUT/PATCH), optional Go `text/template` body
  override, optional HMAC signing secret.
- **Send test** per channel: one real delivery attempt of a synthetic event
  using the *saved* config, reporting a key-free ok/detail/latency result.

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
| `FLEET_NOTIFY_ON` | empty = all terminal | CSV of `success`/`failure`/`progress`/`always` |
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
| `FLEET_PUBLIC_URL` | — | absolute base for the log link — env-only |

Precedence: **admin row (when saved) > env vars**. Timing knobs and the
public URL base stay env-derived even under an admin row.

> Do not confuse `FLEET_WEBHOOK_SECRET` (OUTBOUND signing, this page) with the
> per-trigger secrets of INBOUND webhook triggers
> ([EVENT-TRIGGERS.md](EVENT-TRIGGERS.md)) — the inbound side is separate and
> deliberately not runtime-editable.

## Honest scope / deferred

- The admin panel covers email + webhook. Web Push (VAPID keys) stays
  env-configured — the keys are deployment identity material bound at boot.
- One deployment-wide config — no per-user or per-task notification routing
  (a task's owner does get per-user push, which is its own system).
- The test button exercises the **saved** config, not unsaved form edits (the
  UI says so next to the result).
