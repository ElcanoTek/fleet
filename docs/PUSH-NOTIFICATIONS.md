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

4. Each user opts in per browser with the **Settings → General → Browser
   notifications** switch. The flow asks for notification permission,
   registers `web/public/sw.js`, subscribes with the server's public key
   (fetched from `GET /push/vapid-public-key` — it is never baked into the web
   build), and stores the subscription server-side. Turning it off
   unsubscribes the browser and deletes the stored row. The switch reads
   **on** only when the server holds this browser's subscription for the
   signed-in user (`POST /push/status`): a subscription the browser holds for
   another account, or one the server has dropped, reads **off** and the
   browser lets it go. Signing out from the account menu (every surface's
   menu goes through the one `signOut`) also lets it go and deletes the
   stored row.

**Default OFF.** With any of the three `FLEET_VAPID_*` vars unset, the push
subsystem does not exist: the `/push/*` endpoints answer `501 Not Implemented`
with a machine-readable `push_disabled` error, the settings switch is greyed
out with a note to ask the operator to set push up, and no trigger fires.

`FLEET_PUBLIC_URL` (shared with the #208 notifier) is reused to build the deep
link a notification click opens (`/orchestrator/tasks/<id>` for task events,
which forwards to `/orchestrator?task=<id>` and opens that task's record).
Without it, clicks open the app origin the subscription was created on. A
click opens the link in an open fleet tab already in that part of the app, or
in a new window (see the click note below).

## Trigger matrix

| Event | Flag (default **on** once keys are set) | Notification |
| --- | --- | --- |
| Task terminal: success | `FLEET_PUSH_ON_TASK_COMPLETE` | `✓ Task complete: <name> (<duration>)` |
| Task terminal: failure (error, retry-exhausted dead-letter, interrupted) | `FLEET_PUSH_ON_TASK_COMPLETE` | `✗ Task failed: <name> (<duration>)` |
| Task paused for `ask` / mid-run `notify` progress (#510) | `FLEET_PUSH_ON_TASK_COMPLETE` | `⏸ Waiting for your answer: <name>` |
| A user-scoped budget crossed its soft threshold (`internal/sched/budget`) | `FLEET_PUSH_ON_TASK_COMPLETE` | `⚠ Budget alert: user <email> (<window> window)` |
| Chat approval card staged (#292) | `FLEET_PUSH_ON_APPROVAL_REQUEST` | `⚠ Approval needed: <tool name>` (high urgency) |
| An approved call of a tool in `agent_policy.critical_tool_progress` finished ([APPROVAL-PROGRESS.md](APPROVAL-PROGRESS.md)) | `FLEET_PUSH_ON_APPROVAL_REQUEST` | `✓ Done: <card title or tool name>` / `✗ Not applied: …` (normal urgency) |
| The automatic continue after approvals was skipped over its hourly cap or a full queue ([RESUME-AFTER-APPROVAL.md](RESUME-AFTER-APPROVAL.md)) | `FLEET_PUSH_ON_APPROVAL_REQUEST` | `⏸ Waiting for you: the chat did not continue on its own` (high urgency) |

Notes on honest scope:

- Task events push to the task **owner** (`created_by` → the account email).
  Ownerless tasks (e.g. created without an authenticated user) have no push
  audience and only fire the deployment-wide email/webhook channels.
- The `FLEET_NOTIFY_ON` status filter (email/webhook, #208) applies to the
  push channel too — the channels share one `Notify()` pipeline.
- The agent's mid-run `notify` tool shares the progress status with `ask`
  pauses, so both render the `⏸ Waiting for your answer` title.
- A budget soft alert also travels as a progress event (so the
  `FLEET_NOTIFY_ON` filter and the webhook treat it like any other heads-up),
  but it carries `Kind: budget_alert` and pushes as `⚠ Budget alert: …`:
  nothing is waiting on an answer. It pushes to the budget's principal only
  for a user-scoped budget. It carries no link, so a click brings an open
  fleet window forward, or opens the app root.
- The approval push fires for approvals staged **by an agent turn**. The
  user-initiated "promote to scheduled task" card does not push — the user is
  looking at it when it appears.
- Chat notifications open their conversation: the link is `/chat?c=<id>`
  (under `FLEET_PUBLIC_URL`, or relative to the app), which the chat page opens
  on load, and the pending card re-hydrates there.
- A click goes to the notification's link, task or chat alike, moved onto
  the origin the browser subscribed from (`FLEET_PUBLIC_URL` may name another
  host, but the session cookie lives on this one). The service worker
  navigates the most recently used fleet tab it controls that is already in
  the link's part of the app (the Operations Center for a task, Chat for a
  conversation); navigating reloads that tab. It never takes over a tab
  elsewhere, where an unsent message would be lost: with no matching tab it
  opens the link in a new window. The browser grants one window action per
  click, so the tab is chosen before that action is spent, and the tab is
  navigated even if focusing it fails. The worker claims the tabs already
  open when it activates, and skips waiting so an update applies without
  closing every tab. A link to the app root just brings fleet forward.
  Deferred: telling an open tab to show the record in place, without a
  reload.

## HTTP surface (chat server, auth + member gated)

| Route | Method | Behavior |
| --- | --- | --- |
| `/push/subscribe` | POST | Upsert the caller's `PushSubscription` (endpoint + keys), keyed on the endpoint. 204. |
| `/push/unsubscribe` | DELETE | Delete the caller's row for the endpoint (owner-scoped). Idempotent 204. |
| `/push/status` | POST | `{"subscribed": bool}`: whether the caller holds the row for the endpoint (owner-scoped read; the endpoint rides in the body because it is a capability URL). |
| `/push/vapid-public-key` | GET | `{"key": "<VAPID public key>"}` — non-secret. |

All four return `501` with `{"error":"push_disabled", ...}` when
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
- **A browser is not an account.** The browser holds one subscription, but
  the account that stored it is whoever turned notifications on there.
  Sign-out through the account menu (`signOut`) unsubscribes the browser and
  deletes the row, so the next person to use it does not get the signed-out
  account's alerts, and the settings page lets go of a subscription the
  server does not hold for the signed-in user. A session that ends any other
  way runs none of this: one that expires, one revoked server-side, a
  central-Auth back-channel logout, or the sign-out form on the no-access
  page. Until someone opens Settings in that browser or turns notifications
  on there, the previous account's alerts can still arrive in it.
- **Subscription hygiene:** a `404`/`410` from the relay deletes the expired
  row during the send fan-out.

## Deliberately deferred (out of scope for #292)

- Per-user/per-project preferences (quiet hours, per-event toggles,
  escalation) — the flags above are deployment-wide.
- A Prometheus `fleet_push_notifications_sent_total` counter and the periodic
  `last_active_at` sweep of stale subscriptions (expired endpoints are
  already reaped on send).
- Deep links into the exact card (#504/#508 follow-ups; chat pushes now
  open their conversation, and task pushes the task's record) and
  schedule missed/blocked events.
- Email/Slack/Teams as additional `notify.PushSender`-style per-user
  backends.
