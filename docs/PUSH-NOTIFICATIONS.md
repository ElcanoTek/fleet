# Browser push notifications (Web Push)

fleet can alert a user's browser when work needs them — a task finished or
failed, an approval card is waiting, or a paused task wants an answer — even
when the fleet tab is backgrounded or closed (#292). It uses the standard
**Web Push API** (RFC 8030/8291/8292 via `github.com/SherClockHolmes/webpush-go`):
no native app, no mobile SDK, no third-party account. Chrome, Edge, Firefox,
and Safari 16.4+ are covered.

Per Brad's routing note on #292, Web Push is **one backend of the existing
notifier** (`internal/notify`), not a parallel trigger path: task lifecycle
events flow through the same `Notify()` fan-out as email/webhook, carrying a
new per-user `Audience` field (the task owner's email) that only the push
channel consumes.

## Setup (operator)

1. Generate a VAPID key pair (prints ready-to-paste env lines; boots nothing):

   ```sh
   fleet generate-vapid-keys
   ```

2. Add the three lines to your fleet env-file, editing the contact to a real
   operator address:

   ```ini
   FLEET_VAPID_PUBLIC_KEY=<base64url uncompressed P-256 public key>
   FLEET_VAPID_PRIVATE_KEY=<base64url P-256 private key>   # SECRET — host-side only
   FLEET_VAPID_CONTACT=mailto:you@example.com
   ```

3. Restart fleet. The startup log prints `web push: enabled`.

4. Each user opts in per browser: **Settings → Connections → Browser
   notifications → Enable notifications**. The flow asks for notification
   permission, registers `web/public/sw.js`, subscribes with the server's
   public key (fetched from `GET /push/vapid-public-key` — it is never baked
   into the web build), and stores the subscription server-side. Disable
   unsubscribes the browser and deletes the stored row.

**Default OFF.** With any of the three `FLEET_VAPID_*` vars unset, the push
subsystem does not exist: the `/push/*` endpoints answer `501 Not Implemented`
with a machine-readable `push_disabled` error, the settings card explains what
the operator must run, and no trigger fires.

`FLEET_PUBLIC_URL` (shared with the #208 notifier) is reused to build the deep
link a notification click opens (`/orchestrator/tasks/<id>` for task events).
Without it, clicks open the app origin the subscription was created on.

## Trigger matrix

| Event | Flag (default **on** once keys are set) | Notification |
| --- | --- | --- |
| Task terminal: success | `FLEET_PUSH_ON_TASK_COMPLETE` | `✓ Task complete: <name> (<duration>)` |
| Task terminal: failure (error, retry-exhausted dead-letter, interrupted) | `FLEET_PUSH_ON_TASK_COMPLETE` | `✗ Task failed: <name> (<duration>)` |
| Task paused for `ask` / mid-run `notify` progress (#510) | `FLEET_PUSH_ON_TASK_COMPLETE` | `⏸ Waiting for your answer: <name>` |
| Chat approval card staged (#292) | `FLEET_PUSH_ON_APPROVAL_REQUEST` | `⚠ Approval needed: <tool name>` (high urgency) |

Notes on honest scope:

- Task events push to the task **owner** (`created_by` → the account email).
  Ownerless tasks (e.g. created without an authenticated user) have no push
  audience and only fire the deployment-wide email/webhook channels.
- The `FLEET_NOTIFY_ON` status filter (email/webhook, #208) applies to the
  push channel too — the channels share one `Notify()` pipeline.
- The agent's mid-run `notify` tool shares the progress status with `ask`
  pauses, so both render the `⏸ Waiting for your answer` title.
- The approval push fires for approvals staged **by an agent turn**. The
  user-initiated "promote to scheduled task" card does not push — the user is
  looking at it when it appears.
- Chat has no per-conversation deep link yet, so approval notifications open
  the app root; the pending card re-hydrates on load.

## HTTP surface (chat server, auth + member gated)

| Route | Method | Behavior |
| --- | --- | --- |
| `/push/subscribe` | POST | Upsert the caller's `PushSubscription` (endpoint + keys), keyed on the endpoint. 204. |
| `/push/unsubscribe` | DELETE | Delete the caller's row for the endpoint (owner-scoped). Idempotent 204. |
| `/push/vapid-public-key` | GET | `{"key": "<VAPID public key>"}` — non-secret. |

All three return `501` with `{"error":"push_disabled", ...}` when
unconfigured. The browser reaches them through the Next.js proxies under
`/api/push/*` (session cookie + origin-checked like every mutating route).

## Security posture

- **Payloads are low-detail by design.** A push carries a title, an optional
  short body, and a deep-link URL — never raw model output, tool arguments,
  message content, or a secret. Payloads are additionally end-to-end
  encrypted to the browser (RFC 8291), so the push relay (FCM/Mozilla/Apple)
  never sees plaintext; the low-detail rule holds anyway.
- **The VAPID private key is a host-side secret.** It lives only in the
  operator env-file, is printed exactly once by `fleet generate-vapid-keys`,
  and never enters the sandbox, the model context, a client, or a log line.
  The public key is non-secret.
- **Subscriptions are per-user.** Every subscribe/unsubscribe is scoped to
  the authenticated email; a user can only manage (and receive on) their own
  subscriptions. Endpoint URLs are capability URLs, so log lines reduce them
  to their host.
- **Subscription hygiene:** a `404`/`410` from the relay deletes the expired
  row during the send fan-out.

## Deliberately deferred (out of scope for #292)

- Per-user/per-project preferences (quiet hours, per-event toggles,
  escalation) — the flags above are deployment-wide.
- A Prometheus `fleet_push_notifications_sent_total` counter and the periodic
  `last_active_at` sweep of stale subscriptions (expired endpoints are
  already reaped on send).
- Per-conversation / per-run deep links into the exact card (#504/#508
  follow-ups) and schedule missed/blocked events.
- Email/Slack/Teams as additional `notify.PushSender`-style per-user
  backends.
