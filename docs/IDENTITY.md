# One identity table

Issue #2049, milestone M4. Decision record: [ADR-0105](adr/0105-one-identity-table.md),
which amends ADR-0041, ADR-0074 and ADR-0095. Builds on
[ONE-DATABASE.md](ONE-DATABASE.md) (ADR-0098).

## What shipped

**One row per person.** Chat and the Operations Center read the same table,
`core.users`, in schema `core` of the one database. Chat migration 099 moved the
chat `users` table there (with its side tables `external_access_state`,
`external_auth_epochs`, `external_logout_events`, `external_team_renames`) and
added:

| column | meaning |
|---|---|
| `id` | stable UUID; what `tasks.created_by` and `datasets.created_by_user_id` record |
| `ops_scope` | Operations Center access of a non-admin: `none` \| `view` \| `run` |
| `last_login` | the Operations Center's sign-in stamp |

`core.identity_merge` records one-time identity steps (below).

**The model.** `role` admin | member | viewer, plus `ops_scope`:

| account | Operations Center | legacy name |
|---|---|---|
| role admin (enabled) | everything (`PermissionAdmin`), scope ignored | `admin` |
| scope run | create_task, view_tasks, view_logs | `client` |
| scope view | view_tasks, view_logs | `readonly` |
| scope none | not a member (403 `not_a_member`) | — |
| disabled (central Auth revoked) | nothing, on either plane | — |

On `/prompts` (the Prompt Library, shared with chat) an enabled account without
Operations Center access — or with only view — is authorized by its Fleet role:
member as run-like, viewer as view-like, as the old chat-role seam did.

**Code.** `internal/identity` holds the model, the queries (all
schema-qualified: `core.users`), `MergeLegacySchedUsers` and its pure mapping
`MapLegacy`. The scheduler's `db/users.go` is a thin view over it
(`models.User.Role` is the legacy name, so `RolePermissions` and
`userHasPermission` are unchanged). The chat store keeps its user functions —
team logic and the account delete cascade through chat tables — over
`core.users`, and gained `SetUserAccess` (role, team and scope in one
transaction). `TestIdentityTablesAreSchemaQualified` fails on a bare `users` in
the stores' Go code.

**Gone:** the shadow sync (`EnsureUserWithRole`, `EnsureAdminUser`,
`opsAdminsService`, `httpapi.OpsAdmins`/`WithOpsAdmins`), the identity seams
(`SetChatMemberRoleProvider`, `SetChatSessionEpochProvider`,
`SetExternalSessionEpochProvider`, `SetChatAccountsProvider`, the `memberLookup`
override), and the Operations Center password bearer: `POST /auth/login` and
`POST /auth/logout` on the orchestrator, `/api/orchestrator/auth/login|logout`
on the gateway, `users.session_token`. Manifest, the only external client,
uses `X-API-Key` only and is unaffected.

**Kept on purpose (step 9, usage seams).** `SetChatUsageProvider` and
`SetChatUserDayUsageProvider` stay in-process functions: they are chat's usage
data (`chat.turn_metrics`) read through the chat store's own queries, not
identity, and keeping them avoids a sched → chat-schema dependency M5 and the
migration-runner phase would have to undo. The adoption report's seat roster,
which was identity, now reads `core.users` directly.

## The merge

`identity.MergeLegacySchedUsers(db, "sched")` runs on every scheduler store
open (`sched/db.Init`); the first call
that finds both stores migrated does the work, later calls read the marker. One
transaction: lock the old table (ACCESS EXCLUSIVE) and `core.users` (SHARE ROW
EXCLUSIVE, so a concurrent account create waits), fold every row, rename the
old table `sched.users_premerge_m4`, count dangling creators, write the marker
with the report. A failure rolls back whole and the next open retries.

| before | after |
|---|---|
| chat admin (± any sched row) | role admin |
| chat member/viewer + sched client, enabled | role unchanged, scope run |
| chat member/viewer + sched readonly, enabled | role unchanged, scope view |
| chat X + no sched row, or a disabled sched row | scope none |
| sched admin + chat member/viewer | role unchanged, scope **run**; reported (fail closed, ADR-0074) |
| sched-only account | new row keyed by the lowercased username, role **viewer**, scope mapped (an admin: run, reported), sched UUID, password hash, `created_at` and `last_login` kept (`updated_at` is the merge), enabled as it was; reported |
| sched row with an unknown role (the column had no CHECK), judged exactly as stored: a padded or re-cased name (`" admin"`, `ReadOnly`) is unknown too | scope none; reported |
| two sched usernames that normalize to one email (case, spaces) | one becomes the account — the username that already is the email, else the oldest — keeping its UUID; the others are reported as duplicates: their tasks no longer resolve, and their prompts, budgets and audit labels stay under the old username (quarantined: no account reaches them, nothing is folded into the winner) |

**Username-keyed rows move with the account.** A few scheduler tables key
rows by the sched username as text rather than by UUID. When the merge
normalizes a legacy username to the account's email (trimmed, lower-cased:
`Brad@X.com` → `brad@x.com`), it renames those rows in the same transaction,
for exactly the usernames it changed (API-key labels, `admin` and run labels
are never touched) — and, in a duplicate group, only for the winner's: a
losing duplicate's rows are never moved onto the winning account (they would
hand one person's private prompts and budget limits to another):

- `prompt_library.owner_username`. Prompts are never dropped: if the account
  already has a prompt of the same name (case-insensitively), the moved one
  becomes `<name> (from <old username>)` (then ` 2`, ` 3`, …), and the report
  lists it under `renamed_prompts`.
- `budgets.principal_id` of scope `user`. If the account already has a budget
  for the same window, the two merge into the existing row keeping the
  stricter bound of each kind (a configured bound beats none; of two, the
  lower), and the report lists it under `merged_budgets`.
- Audit labels: `agent_notes.created_by`/`updated_by`,
  `agent_note_proposals.proposed_by`/`decided_by`, `task_feedback.created_by`,
  `task_learned_instructions.activated_by`.

Free text that mentions a username (a task's "stopped by …" reason) is
history and stays as written. The report's `rewritten_principals` counts the
moved rows.

A matched account keeps its chat password and takes the sched UUID, so tasks
and datasets keep resolving to it; the chat session epoch does not move, so no
chat session is signed out. Chat admins that had no enabled Operations Center
admin row gain full Operations Center access (admin is one role) and are
listed in the report.

**Report** (`core.identity_merge.report`, JSON; printed by `fleet serve` and
the scheduler's first open on this release): `merged`, `created[]`,
`demoted_split_admins[]`, `admins_gaining_ops[]`, `unknown_roles[]`,
`duplicate_usernames[]`, `dangling_task_creators`, `dangling_dataset_creators`,
`rewritten_principals`, `renamed_prompts[]` (`{owner, from, to}`),
`merged_budgets[]` (`{principal, window, from}`), `at`. Each entry is `{email, sched_role, chat_role, ops_scope}`. Re-grant a
demoted split admin with `fleet admin add <email>`. `fleet admin merge-report`
prints the stored report (`--json` for scripts; read-only), and `fleet doctor`
advises on each demoted split admin, unknown role and duplicate username in it.

## What changes for operators and users

- **The one-database layout.** The two-database layout was already removed
  (#2049 phase 2, `fleet` refuses its keys). Migration 099 also refuses a
  chat store whose DSN names no schema (public), before changing anything.
- **One password.** The Operations Center has no sign-in of its own. A
  different Operations Center password is dropped at the merge (accounts that
  only had one keep it).
- **Operations-Center-only people can sign in to chat, read-only** (accepted
  widening).
- **API.** `PATCH`/`POST /admin/users` take `ops_scope` (none | view | run) or
  the legacy `ops_role` (either vocabulary; `ops_scope` wins). `ops_role:
  admin` without `role: admin` is now a 400 (the split state cannot be
  written). Responses add `ops_scope` and keep `ops_center_role` and
  `ops_center_admin`. Auth's `ops_role` contract is unchanged (it may also
  send `view`/`run`). The Operations Center's `/auth/me` and `POST /users`
  keep admin | client | readonly. `POST /users` (and `fleet sched user add`)
  makes a new account (role viewer, or admin); for an email that is already
  an enabled account with no Operations Center access (a chat user) it grants
  that account the access instead — password and chat role untouched, except
  that admin makes it a Fleet admin — as adding an Operations Center user for
  a chat email did before. An account that already has access, or is
  disabled, is a 409.
- **CLI.** `fleet admin add|list|rm` act on the one row (`--sched-database-url`
  is accepted and ignored). `fleet sched user add|update|set-role|del|list`
  are deprecated aliases: `del` ends Operations Center access (scope none) and
  keeps the account; `update` sets the one Fleet password; `rename` is refused.
  `fleet import` maps sched users onto accounts (never making an admin).
  The account verbs (`sched user …`, `sched task set-owner`, `sched task
  import`, and `fleet import` of a bundle with sched users or tasks) need the
  identity table. Since the one migration runner (#2049 M6, ADR-0107) every
  store open migrates chat, core and sched together, so it always exists by
  the time a verb runs. (Before M6 such a verb, run first on a fresh database,
  stopped with "the identity table (core.users) does not exist yet".)
- **Deleting an account.** Settings → Admin → Users (`DELETE
  /admin/users/{email}`), `fleet admin rm` and `fleet chat user del` all run
  `accountdelete.Delete`, so they delete the same things, and a later account
  created under the same email starts with none of them:
  - in the database (`store.DeleteUser`, one transaction): the account row,
    its chats, memories, connectors with their stored sign-ins, connector
    preferences, skills (team-visible ones too), personas, personal projects,
    chat orders, sidebar layout and pins; its queued messages, approvals,
    schedules and auto-submit countdowns in other people's chats; its place
    in their multiplayer lists; its Autonomous-mode acceptance, browser push
    subscriptions, the connectors other people shared with it, its central
    sign-in session generations (which ends its fleet sessions from central
    Auth) and its private prompts. Its workspace-shared prompts stay readable
    by everyone, shown as **Shared by a deleted account** (`owner_deleted`
    in the API), and only admins can edit them. Chat-administration work the
    account asked for and no server has picked up is cancelled: a pending
    request fails with a fixed reason and a queued admin-mode message it
    sent is cancelled; a request a server is already carrying out, and an
    admin-mode turn already running, finish as usual
    ([CHAT-ADMINISTRATION.md](CHAT-ADMINISTRATION.md));
  - in the API-key store (`api_keys.json`): the user API keys that act as it
    (each key is also bound to its account's id, see
    [PRINCIPAL.md](PRINCIPAL.md));
  - on disk, under `EMAIL_ATTACHMENT_DIR`: its uploads and its private
    workspace files in multiplayer chats. Its chats' workspace directories are
    reaped by the orphan-workspace sweep. Its uploads go even where a kept
    chat still refers to them (the references the hourly cleanup keeps
    uploads by, `chat.message_uploads`, see
    [UPLOADS-AND-STORAGE.md](UPLOADS-AND-STORAGE.md)): its own chats go with
    it, and an image it sent in someone else's multiplayer chat was its own
    upload, so that chat keeps the message without the image — later turns
    replay the chat without it, as they do for an upload the cleanup removed.

  **Work in flight.** The web route first refuses what the deletion would
  refuse (no such account: 404; a team-shared project still owned: 409),
  without touching anything. Then it stops the account's work in this
  process before the rows go: its requests are cancelled and new ones
  refused (`not_a_member`) until the deletion is over, and the turns it runs
  — in its own chats and as a multiplayer participant — are cancelled; it
  waits up to 15 seconds for both. No turn starts for the account once this
  begins: a request about to start one is refused (`not_a_member`), and the
  running turns are cancelled after the account's requests have ended, then
  looked for once more. It answers 503 and deletes nothing if its
  own request ends first (the client left, or the administrator deleting is
  being deleted) — though by then the account's requests in flight and its
  turns have already been cancelled. Once the work is stopped the deletion
  runs to its end even if the client leaves, within two minutes. If the
  deletion then fails (a project shared with a team after the preflight, a
  database error, the two minutes), the account's requests were already
  refused for its duration and its turns cancelled; the account itself is
  untouched. Every write of a row the deletion removes by the account's
  address takes a key-share lock on the account's row first, before any chat
  row — the order the deletion takes them in — so a write still made for
  the account by a turn or request in this process, or by a server while
  the CLI deletes the account, either commits before the deletion starts
  (and is removed by it) or waits for it and is refused
  (`store.lockAccountTx`):
  chats (created, branched, filed into a project), projects, memories
  (personal and project, and the proposals a turn makes), skills, personas,
  connectors and connector sign-ins, connector preferences, chat orders,
  the sidebar layout, the Autonomous-mode acceptance, browser push
  subscriptions and private prompts (`store.withAccountRow`, and for prompts
  the Operations Center's `lockPromptOwnerTx`); queued, direct and
  Stop-recorded messages (`EnqueueInput`, `ClaimDirectInput`,
  `CancelInputKey`), approval cards (`CreateApproval`) and auto-submit
  countdowns (`CreateUIAutoSubmit`); and every write checked by
  `lockAndVerifyOwnedOrMultiplayerTx` (schedules created or fired,
  approvals decided). For the writes in a chat the lock is taken for every
  sender, the chat's owner included, and an address with no account is
  refused unless it owns the chat (a chat a webhook started may belong to
  one, and can still write). An approval resume and a countdown that fires
  queue a message for the person behind the card while holding only the
  chat's row, so the deletion first locks every chat in which the account
  has an approval card or a countdown: such a message is written before the
  deletion reads the queue, and removed with the rest, or after it, when its
  cards are gone. These writes do not need the account and are left as they
  are: a connector shared with an address and an address added to a
  multiplayer chat's participants (both allowed before its account exists);
  a central sign-in session generation (written at central sign-in, also
  before the account exists, and by a central sign-out, which only ends
  sessions); an administrator's message queued in a chat
  (`QueueAdminMessage`: the row is the chat owner's and is written under the
  chat's row lock, so deleting the owner deletes it with the chat); and the
  one-time legacy import. A guard test
  (`TestAccountKeyedWritersTakeTheAccountLock`) fails when a new insert into
  such a table takes neither the lock nor a reviewed exemption. A write
  refused this way while its request runs is answered 403 `not_a_member`,
  as every later request of the account is. One consequence for webhook
  chats: Connector Studio in a chat a webhook started for an address with
  no account now gets a tool error instead of creating a connector for that
  address.

  **Steps left.** The database deletion alone decides whether the account is
  deleted. A later step that does not complete (the key store, a file) is
  reported — and so is work still running after the 15 seconds: the web
  route answers 200 `{"warnings": [...]}` instead of 204
  (not an error status — the account is gone, and a retry would only get
  404), and Settings → Admin → Users shows the warnings above the table; the
  CLI verbs print them and exit 6. The CLI verbs use the key store and
  attachment directory of the `fleet.service` unit (as `fleet sched apikey`
  does), stop with nothing deleted when the key store cannot be read, and
  report an attachment directory that does not exist.

  **The hourly maintenance pass** (`RunMaintenance`, not the post-turn pass)
  removes what names
  an address with no account: rows in the tables only a signed-in account
  writes (Autonomous-mode acceptances, push subscriptions, auto-submit
  countdowns, the memory graph, private prompts not owned by an API key or
  the admin key, a participant's queued messages, approvals and schedules in
  someone else's chat, and central sign-in session generations older than
  the longest session they could still back — only addresses stored in
  normalized form, so the prompts the identity merge quarantined under a
  losing duplicate username stay; of the queue, approvals and schedules
  only live rows — waiting, pending, active or paused — are read), upload and private workspace
  trees under the attachment directory whose digest is no account's (each
  checked once more, for its own digest, right before it goes, and moved
  aside before it is removed, so an account created under the address in
  the meantime keeps the files it wrote; a tree left set aside by an
  interrupted removal goes on the next pass), and the in-memory
  approve/deny-all decisions of such addresses. That covers
  accounts deleted before deletion reached these, a deletion from the CLI
  while the server runs (a turn in flight can write a private workspace tree
  back as it ends, and the CLI cannot reach the server's memory), and a CLI
  that resolved another attachment directory. Connector share grants are not
  swept: granting a connector to an address before its account exists is
  allowed.

  **Kept on purpose:** usage records (`turn_metrics`); the
  chat-administration audit log, finished admin requests and administrators'
  notes on chats; admin-set budgets —
  a user budget is matched by email, so the limit and the spend already
  counted in its current window apply to a later account under the same
  email until an admin removes the budget; authorship labels on workspace
  data; Operations Center tasks, their spend records (`task_iterations`) and
  datasets, and the uploader records of staged task uploads
  (`temp_uploads/.uploaders`, which age out with the uploads) — they record
  the account's id, which a later account does not share; and central Auth's
  last applied state for its subject. A webhook trigger whose notify address
  (`NotifyUser`, in the bundle's trigger configuration) names a deleted
  account keeps creating chats under that address — operator
  configuration, which deletion does not edit — and a later account created
  with that email sees them. When you delete an account, check the bundle's
  webhook triggers for its address and point them at another account (or
  remove them) before the address is given to someone else. Team-visible skills go with the account while
  workspace-shared prompts stay: a skill is loaded by the agent into other
  people's turns as instructions they do not see each time, so one left
  without an author would keep steering turns with nobody to answer for it;
  a prompt is text a person reads in their draft before sending it.

  **Not foreign keys.** These tables match an account by its email without
  a foreign key to `core.users`, and the deletion lists them by hand. A
  foreign key on the raw email would turn any writer that normalizes the
  address differently into a failed write, and existing rows of accounts
  deleted earlier would make adding a validated one fail at upgrade; so the
  list is kept honest by tests instead. `TestEveryAccountColumnHasADeletionRule`
  (`internal/store/account_delete_test.go`) lists every column that can name
  an account with what deletion does to it and why, and fails on a new one
  until it is listed; `TestDeleteUserLeavesNoRowNamingTheAccount` seeds every
  deleted or scrubbed column and checks nothing names the account afterwards;
  `TestEveryAccountDeletionGoesThroughDelete` fails on code outside
  `accountdelete` and the store that reaches `DeleteUser` or deletes from the
  identity table.
- Grants are exact strings: `admin`, `client`, `readonly`, `none`, `view`,
  `run`. Padded or re-cased values (`" none "`, `Admin`) are refused, not
  trimmed.
- **Boot seed.** `FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS` makes each email a Fleet
  admin (creating the account, with no usable password, if missing): a
  bootstrap admin is a chat admin too.
- **Web.** Settings → Admin → Users reads and writes `ops_scope`.
- **Backup/restore.** `fleet backup` (`--db=all`, the default) is now one
  file, `fleet-all-<stamp>.dump`: one `pg_dump` of `chat`, `core` and `sched`,
  so one snapshot in which the accounts and the rows naming them agree;
  `fleet restore --db=all` restores it in one transaction (the only fully
  self-contained restore). Since #2049 M6 it is the only restore:
  single-store restores (`--db=chat` / `--db=sched`) and dumps from before
  the one identity table are refused. Per-store dumps (`--db=chat` /
  `--db=sched`) still include `core`. See [BACKUP_RESTORE.md](BACKUP_RESTORE.md).

## Rolling back

The chat store has no down migrations, so going back to the release before
this one is a hand step. On a release with the one migration runner (#2049 M6),
first go back to the release before it (docs/MIGRATIONS.md, "Rolling back M6"),
then: stop fleet, run this SQL against the one database
(as the database owner), then start the previous binary. It removes the
accounts the merge created (Operations-Center-only people), restores the
scheduler's users table as it was at the upgrade, strips the new columns and
moves the identity tables back into `chat`. Changes made to accounts after the
upgrade (roles, Operations Center access) are not carried back. The upgrade
rehearsal runs exactly this block.

```sql
-- m4-rollback-begin
DELETE FROM core.users WHERE email IN (
  SELECT e->>'email' FROM core.identity_merge m,
    jsonb_array_elements(coalesce(m.report->'created', '[]'::jsonb)) e);
ALTER TABLE sched.users_premerge_m4 RENAME TO users;
ALTER TABLE core.users DROP COLUMN id, DROP COLUMN ops_scope, DROP COLUMN last_login;
ALTER TABLE core.users SET SCHEMA chat;
ALTER TABLE core.external_access_state SET SCHEMA chat;
ALTER TABLE core.external_auth_epochs SET SCHEMA chat;
ALTER TABLE core.external_logout_events SET SCHEMA chat;
ALTER TABLE core.external_team_renames SET SCHEMA chat;
DROP TABLE core.identity_merge;
DROP SCHEMA core;
DELETE FROM chat.schema_migrations WHERE version = 99;
-- m4-rollback-end
```

If the merge has not run yet (the scheduler never opened on this release),
`sched.users` is still in place: skip the `DELETE` and the `RENAME`.

## Deviations from the plan

- The chat store's user/team/delete code stayed in `internal/store` (it
  cascades through chat tables) over `core.users`; `internal/identity` owns the
  model, the shared queries and the merge.
- Identity SQL is schema-qualified everywhere instead of adding `core` to the
  pools' `search_path`, which would have changed the DSN contract shared with
  the shell scripts, `psql` and `pg_dump`.
- No `ops_session_token` columns: the bearer is retired instead.
- The merge runs on every scheduler open, not only from `fleet serve`, so a
  CLI verb cannot read a half-migrated identity table.
- The merge also completes from the chat side: every operator verb opens the
  chat store through one helper (`openChatStoreDSN`) that, when the database
  still has an unmerged `sched.users` next to `core.users`, opens the
  scheduler store in the same process (its migrations and the merge) before
  the verb touches an account. Otherwise `fleet admin rm` or `fleet chat user
  del` run first after an upgrade would be undone by the merge that follows.
- A restore never mixes merge states: it is whole-database only (#2049 M6),
  so the dump's accounts, merge marker and the rows naming them come back
  together.
- The merge report is printed in the server's log when the merge runs,
  stored in `core.identity_merge`, shown by `fleet admin merge-report`, and
  advised on by `fleet doctor`.
- `dbschema`'s readiness check (and bootstrap's shell mirror) does not check
  ownership of schema `core`: the chat store creates it as the same role.
- Deferred: `NOT VALID` foreign keys to `core.users(id)` (the merge reports
  dangling creators instead); sched migration dropping
  `users_premerge_m4` (next release).

## Tests

- `internal/identity`: the mapping table (`TestMapLegacy`), both vocabularies,
  the merge on real databases — every mapping row, UUID preservation, the
  report, the rename, the unchanged chat epoch, idempotence, rollback on
  failure, and a concurrent account create waiting for the merge — and the
  schema-qualification lint.
- `internal/sched/handlers/identity_differential_test.go`: every combination
  of the old chat row and Operations Center row, old verdicts against new for
  chat membership, Operations Center membership, the Prompt Library user and
  each permission; the only differences are the documented ones above, each
  asserted.
- `internal/dbschema`: both stores in one database share `core.users`, and
  migration 099 refuses a chat store in public.
- The upgrade rehearsal (`hooks/m4-one-identity-table.sh`): a box on the
  previous release upgraded in place — every pre-upgrade Operations Center
  account signs in the new way with its old role, UUIDs and tasks resolve,
  the rollback hand step above, a re-upgrade, and a restore of a pre-M4 backup.
