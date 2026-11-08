# ADR-0105: One identity table

- **Status:** Accepted
- **Date:** 2026-11-07
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0041](0041-mandatory-session-epoch-claim.md), [ADR-0074](0074-central-auth-owns-fleet-membership-state.md), [ADR-0095](0095-central-auth-fleets-have-no-passwords.md)
- **Issue:** #2049 ("One persistence plane"), milestone M4
- **Design note:** [docs/IDENTITY.md](../IDENTITY.md)

## Context

Until now a person could exist twice: a row in the chat store's `users` (email,
password, role member | viewer | admin, team, enabled) and a row in the
scheduler's `users` (UUID, username, its own password, role admin | client |
readonly, enabled, a password-login bearer). The two were kept in step by a
"shadow sync" (`opsAdminsService`, `EnsureUserWithRole`) that the admin Users
page, the Auth access handler, the boot seed and the admin CLI each drove,
best effort, across two transactions. Five lookup seams carried identity from
one store to the other at request time (chat role, password epoch, OIDC epoch,
account roster, the membership override). ADR-0098 put both stores in one
database, which removes the reason for any of this.

## Decision

1. **One row per person, in `core.users`.** Chat migration 099 moves the chat
   `users` table (and the identity side tables `external_access_state`,
   `external_auth_epochs`, `external_logout_events`, `external_team_renames`)
   into a neutral schema `core`, and adds `id` (a stable UUID), `ops_scope`
   and `last_login`. Both stores read it with schema-qualified names
   (`internal/identity`); `core` is on no `search_path`.
2. **One role plus an Operations Center scope.** `role` is admin | member |
   viewer; `ops_scope` is none | view | run. Admin implies full Operations
   Center access and its scope is ignored. run grants create_task, view_tasks,
   view_logs (the old "client"); view grants view_tasks, view_logs (the old
   "readonly"). A disabled account has no access to either plane. The external
   contracts keep the legacy names: responses carry `ops_center_role`
   (admin | client | readonly | "") next to the new `ops_scope`, requests and
   Auth's `ops_role` accept both vocabularies, and `external_access_state`
   records Auth's legacy names.
3. **The merge.** `identity.MergeLegacySchedUsers` folds the scheduler's
   `users` table into `core.users` once, in one transaction, guarded by a
   `core.identity_merge` marker, then renames the old table
   `users_premerge_m4`. Every scheduler store open tries it (a no-op after the
   first success). An Operations Center admin whose chat role is not admin is
   failed closed to scope run and reported; an Operations-Center-only account
   becomes a chat viewer (an accepted widening) and is reported; chat admins
   that had no Operations Center admin row gain full access (admin is one
   role) and are reported.
4. **No shadow sync, no identity seams.** Role, team and scope are written in
   one transaction (`store.SetUserAccess`); the Auth access handler writes
   enabled, role, scope and team in one transaction
   (`store.ApplyExternalAccess`). The Operations Center reads membership, the
   Prompt Library fallback, both session epochs and the adoption roster from
   the row itself.
5. **The Operations Center password bearer is retired.** `POST /auth/login`,
   `POST /auth/logout` on the orchestrator, the gateway's
   `/api/orchestrator/auth/login|logout`, and `users.session_token` are gone.
   People reach the Operations Center with the Fleet session; programs use
   API keys. (Manifest, the only external client, uses `X-API-Key` only.)
6. **Schema `core` needs the one database.** The two-database layout was
   already removed (#2049 phase 2); migration 099 also refuses a chat store
   in public before changing anything.

## Amendments

- **ADR-0041 (session epoch).** Unchanged derivation —
  sha256(password_hash || session_salt), truncated — now one SQL expression
  (`identity.SessionEpochSQL`) over one row. The Operations Center no longer
  needs an injected seam to check it; a nil seam can no longer silently skip
  the check.
- **ADR-0074 (central Auth owns membership).** "Eventually consistent across
  the two planes, retried" becomes atomic: one transaction, one row, no 503
  "Ops Center provisioning failed". The split-admin rule (Fleet Admin is one
  coherent role; split states fail closed) also governs the migration.
- **ADR-0095 (no passwords under central Auth).** Password rows live in one
  table. The Operations Center has no password login of its own any more, so
  "the Operations Center refuses its password login and bearer" holds by
  construction.

## Consequences

- An Operations-Center-only person can sign in to chat, read-only (accepted).
- A split admin loses Operations Center admin until re-granted with
  `fleet admin add` (reported in the merge's log lines and
  `core.identity_merge`, shown by `fleet admin merge-report`, and advised on
  by `fleet doctor`, with unknown roles and case-duplicate usernames).
- A disabled chat account no longer reaches the Operations Center through an
  enabled sched row (it is one account).
- An Operations Center password differing from the chat password is dropped;
  the chat password is kept (Operations-Center-only accounts keep theirs).
- `fleet sched user` becomes a deprecated alias over the identity table;
  `rename` is refused (the email is the key of ~30 chat columns).
- `POST /users` (admin key) and `fleet sched user add` create an account with
  role viewer (or admin); an existing account without Operations Center
  access is granted it instead (its password kept), and one that has access
  is refused (409).
- Backups: the chat dump carries schema `core`; a pre-M4 chat dump restores
  only into a database without `core`.
- Deferred: foreign keys from `tasks.created_by` / `datasets.created_by_user_id`
  to `core.users(id)` (the merge reports dangling creators instead), dropping
  `users_premerge_m4` (next release), moving chat's usage queries out of the
  chat store (they stay an in-process seam; see docs/IDENTITY.md).
