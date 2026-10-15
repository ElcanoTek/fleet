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

   The contact can be a `mailto:` address as above, a bare address, or an
   `https:` URL. The push library adds `mailto:` itself to anything that is
   not an `https:` URL, so fleet drops a leading `mailto:` (and lowercases an
   `https:` scheme) before handing the contact over: the token it signs names
   the contact with exactly one scheme, which Apple's push service requires.

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

**Rotating the keys.** A browser subscription is bound to the VAPID key it was
made with, so after the operator replaces the `FLEET_VAPID_*` pair every
existing subscription is refused by the push service. Which status a refusal
carries is the push service's choice (RFC 8292 leaves it open): FCM answers
`403`, and other services may answer `401`. fleet keeps those rows rather than
deleting them, and logs `relay … returned status <code>: it refused this
server's VAPID authentication …`, with the push service's reason after the
code when it gives one (Apple's names one in a JSON `reason` field, such as
`BadJwtToken` for a token it does not accept). Only that reason is logged,
never the rest of the response. The line names both causes, because a key pair
that does not match, a bad `FLEET_VAPID_CONTACT` or a wrong host clock are
refused the same way: a refusal that persists after browsers have reopened
fleet points at the configuration, which renewal cannot fix.

Each browser renews its own subscription: on a page load (`PushKeyRenewal` in
the root layout, skipped on `/login` and `/shared` pages) and again each time
the settings switch loads, it compares the key its subscription records with
`GET /push/vapid-public-key`. On a mismatch it unsubscribes, subscribes with
the current key (permission is already granted, so no prompt), and asks
`POST /push/renew` to move the signed-in user's row to the new subscription.
The server moves it only if that user held the old row. A "not yours", a
`400`, or a `403` the server marks as its own refusal (`{"error":"read_only"}`
— a Viewer account cannot renew, as it cannot subscribe — or
`{"error":"not_a_member"}`) makes the browser drop the new subscription too,
so a renewal never signs up an account that did not turn notifications on in
that browser. Any other failure is no answer: offline, a timeout, an expired
session, a `5xx`, and any other `403` — notably the web app's origin check
(`{"error":"forbidden"}`), which a misconfigured public origin turns on for
every request and which says nothing about whose subscription it is. The new
subscription is kept, the endpoints the server has not answered for are
remembered in the browser, and the move is retried from each on the next
check. A settings switch that loads in the meantime, and finds the server
does not hold the new subscription, reads off with a note to reload, and
keeps the subscription rather than letting it go. If the keys rotate again
first, the renewal tries the remembered endpoints, oldest first, before the
current one, and remembers the current one too if that attempt gets no
answer: a move the server made whose answer was lost leaves the row at the
endpoint that renewal made, so each unconfirmed endpoint is kept until the
server answers for it (a definite "not yours" forgets it). The browser keeps
at most the 8 most recent; an older one is forgotten. A retry from an old
endpoint whose move did land is not mistaken for "not yours": the server
answers yes when the signed-in user already holds the new endpoint, so the
row is confirmed where it is and the subscription kept.

The remembered endpoints are a JSON list in `localStorage` under
`fleet.push.pendingRenewals.v2`. The release that first renewed
subscriptions kept a single endpoint under `fleet.push.pendingRenewal`, and
a tab still running that release reads whatever is there as one endpoint,
so the list is never written to that key: an endpoint found there is read
into the list (as the oldest) and the old key removed.

The check, the switch's own reads and replacement, and sign-out's clean-up run
under one browser-wide Web Lock, so two tabs never replace the subscription at
once. The check's and the switch's requests made while holding it give up
after 10 seconds, and sign-out waits at most a second for it before cleaning
up anyway. Sign-out also deletes the rows at every endpoint a renewal the
server never confirmed left remembered, under either key.

Limits: until a browser next opens fleet its notifications do not arrive; a
browser that does not report the key its subscription was made with is not
checked; a browser that refuses to subscribe again is left with no
subscription and its switch reads off; and a browser that never opens fleet
again leaves a refused row behind (the stale-subscription sweep below is still
deferred).

**Default OFF.** With any of the three `FLEET_VAPID_*` vars unset, the push
subsystem does not exist: the `/push/*` endpoints answer `501 Not Implemented`
with a machine-readable `push_disabled` error, the settings switch is greyed
out with a note to ask the operator to set push up, and no trigger fires.

`FLEET_PUBLIC_URL` (shared with the #208 notifier) is reused to build the deep
link a notification click opens (`/orchestrator/tasks/<id>` for task events,
which forwards to `/orchestrator?task=<id>` and opens that task's record).
Without it a task notification carries no link, so its click brings an open
fleet window forward or opens the app root; chat links are relative and open
their conversation either way. A click on a link opens it in an open fleet tab
already in that part of the app, or in a new window (in Safari, which opens
none, in the most recently used fleet tab; see the click note below).

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
- A click goes to the notification's link, task or chat alike, moved onto the
  origin the browser subscribed from (`FLEET_PUBLIC_URL` may name another
  host, but the session cookie lives on this one). The service worker
  navigates the most recently used fleet tab it controls that is already in
  the link's part of the app (the Operations Center for a task, Chat for a
  conversation). Navigating reloads that tab, so anything unsaved in it (an
  unsent message, a half-written task) is lost. A tab elsewhere in the app is
  not reloaded while the browser can open a window: with no matching tab the
  link opens in a new window. Safari (26.5 on macOS, tested) answers that
  request from a notification click with nothing and opens no window, so in
  WebKit (Safari, and every iOS browser) the click waits a second and then
  sends the most recently used fleet tab it controls to the link instead,
  which reloads it. A window that did open late in that second (WebKit is
  reported to answer before one exists; not seen on macOS) is brought forward
  instead. With only a tab the worker does not control (after a hard reload),
  that tab is brought forward but not navigated; with no fleet tab open,
  Safari opens nothing. Chrome and Firefox keep the new window: Chromium also
  answers with nothing when the window it opened shows an error page, so
  reusing a tab there would reload it for nothing. Only Safari on macOS was
  tested; iOS was not. Only top-level tabs count, not a same-origin frame
  inside a page. The browser grants one window action per click, so the tab is
  chosen before that action is spent, and the tab is navigated even if
  focusing it fails. The worker claims the tabs already open when it
  activates, and skips waiting so an update applies without closing every tab.
  A link to the app root, or no link, just brings fleet forward, or opens it.
  Deferred: telling an open tab to show the record in place, without a reload,
  which would keep unsaved work.

## HTTP surface (chat server, auth + member gated)

| Route | Method | Behavior |
| --- | --- | --- |
| `/push/subscribe` | POST | Upsert the caller's `PushSubscription` (endpoint + keys), keyed on the endpoint. 204. |
| `/push/unsubscribe` | DELETE | Delete the caller's row for the endpoint (owner-scoped). Idempotent 204. |
| `/push/status` | POST | `{"subscribed": bool}`: whether the caller holds the row for the endpoint (owner-scoped read; the endpoint rides in the body because it is a capability URL). |
| `/push/renew` | POST | `{old_endpoint, endpoint, keys}` → `{"renewed": bool}`: move the caller's row from the old endpoint to the browser's new subscription after a key rotation. False (nothing changed) unless the caller held the old row; true again if a second tab already moved it. |
| `/push/vapid-public-key` | GET | `{"key": "<VAPID public key>"}` — non-secret. |

All five return `501` with `{"error":"push_disabled", ...}` when
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
- **Subscriptions are per-user.** Every subscribe, unsubscribe, status read
  and renewal is scoped to the authenticated email; a user can only manage (and receive on) their own
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
  on there, the previous account's alerts can still arrive in it. After a key
  rotation, the next account's first page load in that browser also lets the
  previous account's subscription go, since the renewal is not that
  account's to make.
- **Subscription hygiene:** a `404`/`410` from the relay deletes the expired
  row during the send fan-out.

## Deliberately deferred (out of scope for #292)

- Per-user/per-project preferences (quiet hours, per-event toggles,
  escalation) — the flags above are deployment-wide.
- A Prometheus `fleet_push_notifications_sent_total` counter and the periodic
  `last_active_at` sweep of stale subscriptions (expired endpoints are
  already reaped on send).
- Deep links into the exact card (#504/#508 follow-ups; chat pushes now
  open their conversation, and task pushes the task's record when
  `FLEET_PUBLIC_URL` is set) and
  schedule missed/blocked events.
- Email/Slack/Teams as additional `notify.PushSender`-style per-user
  backends.
