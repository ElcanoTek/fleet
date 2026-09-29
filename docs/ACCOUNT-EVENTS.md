# Account events: a signed feed of membership and role changes

Fleet can publish every change to a Chat account's membership or roles as a
signed HTTP event. The feed is generic: Fleet names no receiver, so an identity
provider, an audit sink or a script can subscribe. Elcano's Central Auth uses
it to mirror role changes and removals made in Fleet back into its console
([ADR-0076](adr/0076-fleet-publishes-account-events.md)), but nothing in Fleet
depends on Auth.

The feed is **off by default**. With `FLEET_ACCOUNT_EVENTS_URL` unset, nothing
is queued and Fleet behaves exactly as before.

## Configuration

Set both in the server env file (`/etc/fleet/fleet.env` on a provisioned box),
then restart `fleet.service`:

| Variable | Meaning |
| --- | --- |
| `FLEET_ACCOUNT_EVENTS_URL` | Where events are POSTed. Unset = the feed is off. Must be an absolute `http`/`https` URL. |
| `FLEET_ACCOUNT_EVENTS_SECRET` | HMAC-SHA256 signing key shared with the receiver. **Required** when the URL is set: boot and `fleet validate-config` refuse a URL without it, so events are never sent unsigned. It must also **differ from `FLEET_WEBHOOK_SECRET`**: the two feeds use the same signing scheme and the signed string names neither, so under one key a task-webhook body would verify at the account-events receiver. Boot refuses a shared value. |

The URL is operator-trusted, the same trust class as `FLEET_WEBHOOK_URL`:
there is no SSRF guard, and a loopback receiver (for example an identity
provider on the same box) is allowed. The secret is held host-side and is
never logged, returned by an API or shipped into the sandbox. These two
variables are separate from the task-notification webhook
([NOTIFICATIONS.md](NOTIFICATIONS.md)): that channel is fire-and-forget, while
this one is durable.

## What is an event

The feed covers **Chat accounts** (rows in the chat `users` table). An
Operations Center identity that has no Chat account (an API or automation
user) never emits.

An event carries the account's **full resulting state**, read back from both
the Chat and Operations Center databases after the change, not the values the
admin asked for. If the Ops write fails, the event reports the Ops role the
database still holds. An event is queued **only when the resulting state
differs** from the state before the operation, so re-asserting a role (or
changing only a team, which is not part of the state) publishes nothing.

Writers that publish:

| Source | What |
| --- | --- |
| `admin_ui` | Settings → Admin → Users: create, role / Ops role change, delete |
| `cli` | `fleet admin add/rm`, `fleet chat user add/role/del`, `fleet sched user add/set-role/rename/del`, and the orchestrator admin API's `POST /users` (`ADMIN_API_KEY`) when the username is a Chat account's email |
| `system` | The `FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS` boot seed, when it actually changes a Chat account's Ops role |
| `identity_provider` | A change Fleet applied because its identity provider told it to (the Central Auth provisioning push, [CENTRAL-AUTH-PROVISIONING.md](CENTRAL-AUTH-PROVISIONING.md)) |
| `resync` | `fleet account-events resync` |

The CLI is a separate process that usually holds only one of the two
databases; it opens the other from the deployment env
(`FLEET_CHAT_DATABASE_URL` / `FLEET_SCHED_DATABASE_URL`). If it cannot, the
write still happens and the command prints a warning telling the operator to
run `fleet account-events resync`. A CLI that cannot read the server env file
(root-owned on a provisioned box, with the command run as another user and
`--database-url`) cannot tell whether the feed is on; it says so on stderr
rather than publish nothing silently. The same resync warning (in the server
log) covers the one known gap in the admin UI path: the event is queued after
the change commits, so a crash between the two loses that event until the next
change or a resync.

`chat_role` is the account's stored role. An `ADMIN_EMAILS` bootstrap admin is
an admin because of the env allowlist, not its row, so the feed reports its
stored role (usually `member`), and adding an email to or removing it from
`ADMIN_EMAILS` publishes nothing. That is deliberate: reporting the env grant
as `admin` would invite a provider to write it back into the row, where it
would outlive its removal from the allowlist.

## Wire format

`POST <FLEET_ACCOUNT_EVENTS_URL>` with `Content-Type: application/json`:

```json
{
  "id": "evt_3f2a…",
  "type": "user.access_changed",
  "occurred_at": 1790000000,
  "sequence": 42,
  "source": "admin_ui",
  "actor": "admin@example.com",
  "user": {
    "email": "person@example.com",
    "enabled": true,
    "chat_role": "member",
    "ops_role": "none"
  }
}
```

- `id`: unique per event and stable across retries; dedupe on it.
- `type`: `user.access_changed` or `user.deleted`.
- `occurred_at`: Unix seconds when the change committed on Fleet.
- `sequence`: the outbox row id, monotonic per deployment.
- `source`: see the table above.
- `actor`: the admin who made the change in the UI; empty for the CLI, the
  boot seed, resync and identity-provider changes.
- `user.email`: lowercased.
- `user.enabled`: whether the Chat account is enabled.
- `user.chat_role`: `viewer`, `member` or `admin`.
- `user.ops_role`: `none`, `readonly`, `client` or `admin`. `none` means no
  enabled Operations Center identity (a centrally disabled identity keeps its
  row but reads as `none`).
- For `user.deleted`: `enabled` is `false` and both roles are `""`.

Headers:

- `X-Fleet-Timestamp` and `X-Fleet-Signature`: the scheme in
  [WEBHOOK-SIGNING.md](WEBHOOK-SIGNING.md), byte for byte
  (`v1=hex(HMAC-SHA256(secret, timestamp + "." + raw_body))`). The timestamp
  is fresh on every attempt, so a retry carries a new signature over the same
  body.
- `X-Fleet-Event-Id`: the body's `id`, for receivers that log before parsing.
  Not signed separately; the body is the authority.

A receiver should verify the signature, reject timestamps outside a small
window (5 minutes), and treat any repeat of an `id` as already handled.

## Delivery

Events wait in a durable outbox table in the chat database
(`account_events`, migration 067) and are delivered by `fleet serve`, which
polls every 2 seconds. Rows queued by the CLI are delivered by the running
server too.

- **Order:** one account's events are delivered in order. A newer event for
  an email waits while an older one is retrying.
- **Success:** any 2xx.
- **Rejected:** 400, 409, 410, 413, 415 or 422 means the receiver refused the
  body itself, which retrying the same bytes cannot change. The row is marked
  failed at once (not retried for 7 days), so the account's next event, which
  carries its full state, is not held behind it.
- **Retry:** anything else (including a 3xx; redirects are never followed, so
  a signed body reaches only the configured URL; and 401/403/404, usually a
  secret or URL the operator can fix) and any transport error. The wait starts
  at 5 seconds and doubles to a one-hour ceiling. A shutdown mid-send records
  no attempt and hands the claimed rows straight back.
- **Give up:** 7 days after the event was queued, the row is marked failed and
  the account's next event proceeds. Failed rows are visible in
  `fleet account-events status` and kept 30 days; delivered rows are kept 7.
  The retention sweep keeps running after the feed is switched off, so rows
  from a feed that was on still age out.
- Error messages stored for status name the status code or the host only,
  never the URL's path or query.

## Operator commands

```sh
fleet account-events status   # feed on/off, pending / failed / delivered, oldest pending, most recent failure
fleet account-events export   # every Chat account's current state as JSON Lines ("user" objects); read-only
fleet account-events resync   # queue every Chat account's current state, and user.deleted for known accounts that are gone (source "resync")
```

`resync` knows an account once existed from the identity provider's stored
desired state or from the outbox's own history (rows are kept 7 or 30 days),
so an account deleted with no event queued is reported gone even when the
receiver is not an identity provider — within that retention for one.

`export` works with the feed off and changes nothing, so an operator can
compare Fleet's state with a receiver before switching the feed on. `resync`
refuses while the feed is off. Neither prints the URL or the secret.

## Loop safety with an identity provider

When the receiver is also the identity provider that provisions Fleet (Central
Auth), a change can travel both ways. Three rules stop it from looping:

1. Fleet publishes only real changes. When the provider echoes back a state
   Fleet already holds, Fleet changes nothing and publishes nothing.
2. Changes Fleet applies on the provider's word are tagged
   `source: "identity_provider"`, so the provider can ignore its own echo.
3. The provider should skip a report that matches what it already stores.

And one rule keeps the two sides from undoing each other: a change made in
Fleet is also written into the provider's stored desired state for that
account (its roles, or not-allowed for a deletion; never its version). So when
the provider redelivers a push it already sent (at-least-once), Fleet
re-applies the state it now holds instead of the one before its own change,
and an `identity_provider` event is never a silent revert. This happens only
with the feed on ([ADR-0076](adr/0076-fleet-publishes-account-events.md)).

## Honest scope

- The feed reports Chat accounts only; Ops-only identities and team
  assignments are not part of it.
- Delivery is at-least-once. Receivers must dedupe on `id`.
- There is no UI for the feed; configuration is env-only and needs a restart.
- Fleet does not read anything back from the receiver. Whether a receiver acts
  on an event (Central Auth ignores accounts it has not granted Fleet) is the
  receiver's decision.
