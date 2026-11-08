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
| sched row with an unknown role (the column had no CHECK) | scope none; reported |
| two sched usernames differing only in case | the first wins the UUID; the rest reported |

**Username-keyed rows move with the account.** A few scheduler tables key
rows by the sched username as text rather than by UUID. When the merge
normalizes a legacy username to the account's email (trimmed, lower-cased:
`Brad@X.com` → `brad@x.com`), it renames those rows in the same transaction,
for exactly the usernames it changed (API-key labels, `admin` and run labels
are never touched):

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
  identity table, which the chat store creates: run first on a fresh database,
  or mid-upgrade before the chat store migrated, they stop with "the identity
  table (core.users) does not exist yet" and ask you to start `fleet serve`
  once (or run a chat verb such as `fleet chat user list`). They do not migrate
  the chat store themselves. `fleet serve` migrates chat before sched.
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
  self-contained restore). Per-store dumps (`--db=chat` / `--db=sched`)
  include `core`. Restoring `--db=chat` brings `core` back; `--db=sched`
  restores only `sched` (core is the chat store's) and refuses when a task
  or dataset creator would name no account here — restore chat first, or
  `--db=all`. `--db=chat` likewise refuses accounts that would orphan live
  sched creators; `--allow-unresolved-creators` overrides both. Older
  two-file backups restore one store at a time, chat first. A
  chat dump from before this release restores only into a database without
  `core`. See [BACKUP_RESTORE.md](BACKUP_RESTORE.md).

## Rolling back

The chat store has no down migrations, so going back to the release before
this one is a hand step: stop fleet, run this SQL against the one database
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
