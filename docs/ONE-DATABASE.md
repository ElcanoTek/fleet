# One persistence plane: one database, a schema per store

Issue #2049: phase 1 (milestones M1 + M3, plus the migration tool) and
phase 2 (the legacy two-database layout and the migration tool removed).
Decision record: [ADR-0098](adr/0098-one-database-two-schemas.md), which
supersedes [ADR-0005](adr/0005-separate-chat-and-sched-databases.md).

fleet has two stores: the **chat store** (`internal/store`: conversations,
turns, chat users, …) and the **scheduler** (`internal/sched/db`: tasks, runs,
API keys, orchestrator users, …). They used to need two Postgres databases.
They now live in **one** database, the chat store in schema `chat` and the
scheduler in schema `sched`, configured by one DSN — the only configuration
fleet accepts:

```
DATABASE_URL=postgres://fleet:…@127.0.0.1:5432/fleet?sslmode=disable
```

## What shipped

**Schema mode (M1).** `config.ResolveDSNs` (`internal/config/dsn.go`) is the
one place the DSNs are decided: `DATABASE_URL`, else the `DB_*` parts
(default user and database `fleet` on `localhost:5432`). It gives each
store's DSN its schema as the connections' only `search_path`, through the
libpq/pgx startup option `options=-csearch_path=<schema>`. The edit is in
place — only the `options` query parameter changes — so the shell mirror in
`scripts/lib/idle.sh` (`fleet_resolve_dsn`, used by `update.sh`'s idle gate
to psql into the stores) produces the same bytes;
`TestIdleLibResolveDSNParity` pins the two.

**Where a DSN connects is pgx's answer.** Every question about a DSN's target
(host, port, database, role, password) is answered by `config.ParseConnTarget`,
which is `pgconn.ParseConfig`, the reading the stores' own connections use.
That covers the "same database?" identity (`config.DSNIdentity`, used by the
distinct-stores guard and `fleet status`) and the host, port and role that
`fleet backup`/`restore` hand to pg_dump/pg_restore. Only *rewriting* a DSN
(the schema option) edits the string itself. As in pgx:

- a query parameter (`?host=`, `?port=`, `?dbname=`, `?user=`, `?password=`)
  overrides the URL's own part, and the last occurrence wins;
- what the DSN leaves out comes from the `PG*` environment (`PGHOST`,
  `PGPORT`, `PGDATABASE`, `PGUSER`, a service file), `.pgpass` for the
  password, then the defaults (the default socket directory, port 5432, the OS
  user, and the role name as the database). This is deliberate, because the
  process asking connects with that same environment;
- a DSN listing several hosts (`host=a,b`, `postgres://a,b/db`) has no
  identity and is refused wherever a target is needed: fleet needs one server.

The shell readers of a DSN either follow pgx or refuse: `fleet_dsn_role_db`
(`scripts/lib/pgprovision.sh`) refuses a DSN whose query overrides its role or
database, and bootstrap's `env_dsn_password` takes a `?password=` override.

Because the schema travels inside the DSN, every consumer agrees without
deciding anything: `fleet serve`, the MCP broker, `fleet validate-config`,
`fleet eval`, every admin verb (`status`, `import`, `migrate`, `backup`,
`restore`, `cache-stats`, `diagnose`, the idle checks) and psql/pg_dump.

An admin verb's `--database-url` / `--*-database-url` is resolved by
`DSNs.OverrideFor`: an override that names its own schema
(`?options=-csearch_path%3D<schema>`) is used verbatim; any other override
gets the store's schema, whatever database it names (a DR host, a restored
copy). So a flag can never land a verb in the other store, or in `public`
unless it says so.

The migration runner (one since M6, `internal/migrations`, ADR-0107) first
calls `internal/dbschema.Ensure`, which creates the schemas `chat`, `core` and
`sched` that do not exist yet, each under a transaction advisory lock (`CREATE
SCHEMA IF NOT EXISTS` races). It refuses one shape first: a database whose
`public` schema holds a fleet store — `DATABASE_URL` pointed at a database of
the removed two-database layout — because empty new schemas would boot an
apparently empty fleet over that data. The check runs whenever the runner has
work (a current database is one read of the ledger, which a legacy database
does not have) and stays as a backstop after phase 2.

Verified, not assumed: no legacy migration uses an extension, and
`gen_random_uuid` is in `pg_catalog`, which Postgres searches first whatever
the `search_path`. Legacy migrations run with their store's schema as the
transaction's `search_path`; new ones name every object with its schema.
`TestBothStoresInOneDatabase` (`internal/dbschema`) opens both stores on one
scratch database from one DSN, creates a user in the one identity table, checks
every table landed in its store's schema (nothing in `public`, one ledger in
`core`), and reopens both with no migration to run.

`EnsureDistinctDatabases` became `EnsureDistinctStores`: what it refuses is
two DSNs that would put both stores in the **same schema**. `ResolveDSNs`
never produces such a pair; the guard stays as a cheap backstop at boot, in
the broker and `validate-config` (a `Config` assembled some other way), and in
`fleet import`, where two `--*-database-url` flags naming one schema are
refused for a bundle with both sections.

**One DSN (M3).**

- `scripts/bootstrap.sh` provisions one role and one database (default
  `fleet`/`fleet`; `--db-name`, `--db-user`, `--adopt-existing-db`) and writes
  only `DATABASE_URL` (names must be lower-case). External mode reads
  `DATABASE_URL` from the environment, else the env file (read as fleet reads
  it: `KEY=""` is unset), and refuses a database whose `public` holds a fleet
  store, and a role that cannot run the stores: for each of `chat` and
  `sched`, CREATE on the database if the schema does not exist yet, or — if it
  does — OWNERSHIP of the schema and of every table, sequence and view in it
  (the refusal names the owners it found); with
  `FLEET_DB_SUPERUSER_URL` it creates the DSN's database if missing and grants
  the DSN's role CONNECT + CREATE on it.
- The Helm chart's in-cluster Postgres creates one database, `fleet`
  (`POSTGRES_DB`), and the control plane gets `DATABASE_URL`.
- `fleet backup` dumps one schema per store (`pg_dump -n`); `fleet restore`
  reads the dump's table of contents and refuses a dump whose tables are not
  in the target store's schema, before anything is dropped.
- `fleet status` reports the layout of the DSNs it checked;
  `validate-config` names it and runs the same read-only readiness check
  every store open runs (no fleet store in `public`; each store schema
  missing with CREATE on the database, or present and owned by the role with
  everything in it) — the check bootstrap's shell gate mirrors.

## Phase 2: the legacy layout removed

Shipped once every box (all twelve that matter) had been moved into one
database with `fleet db consolidate`, its old `chat` and `sched` databases and
the `chat` role dropped, and its env file reduced to `DATABASE_URL` (the owner
confirmed each). On those boxes the database `fleet` is owned by role `sched`;
nothing in fleet assumes the role's name — it only ever uses `DATABASE_URL`.

**What went:**

- The legacy two-database layout: `resolveLegacyDSNs` and `LayoutOf` /
  `LayoutLegacy` (`internal/config/dsn_legacy.go`), the `Layout` type, the
  boot notice, and the shell mirror `fleet_resolve_dsn_legacy`.
- The per-store keys `FLEET_CHAT_DATABASE_URL`, `FLEET_SCHED_DATABASE_URL` and
  `SCHED_DATABASE_URL`. They are not silently ignored: `config.ResolveDSNs`
  fails with an error naming them and `DATABASE_URL` (so `fleet serve`, the
  MCP broker, `validate-config`, `eval` and every DSN-using CLI verb refuse,
  and `fleet restart`/`stop` refuse to wait on an idle check they cannot
  run), `fleet_resolve_dsn` in `scripts/lib/idle.sh` refuses the same way
  (update.sh's idle gate reports it as blocking work), `fleet doctor` fails
  the box, and `bootstrap.sh` refuses to run. The keys stay in the env-file
  allowlist only so a value there reaches that refusal, and in the broker's
  stripped-env list because they would carry a password.
- `fleet db consolidate`, `fleet db layout` and the hidden
  `fleet db dsn-identity` — the whole `fleet db` verb. Its only remaining
  user, the shell's `fleet_dsn_identity` (and its mirror
  `fleet_dsn_identity_sh`, `fleet_same_database`), served the legacy layout
  decision and went with it. `config.DSNIdentity` stays: the distinct-stores
  guard and `fleet status` use it.
- The `PublicStoreProbe` behind `DSNs.OverrideFor`: an override that names no
  schema now always gets the store's schema.
- Bootstrap's per-store flags (`--chat-db-name`, `--chat-db-user`,
  `--sched-db-name`, `--sched-db-user`, `--adopt-existing-chat-db`,
  `--adopt-existing-sched-db`) and inputs (`CHAT_DB_*`, `SCHED_DB_*`), its
  layout decision and both legacy provisioning branches; `pgprovision.sh`'s
  per-store password placeholders (the redacting runner now takes one
  password).
- update.sh's "consolidate" recommendation and doctor.sh's legacy branch.
- The Helm chart's legacy-release detection (`fleet.postgresLegacyLayout`) and
  its `chat-url` / `sched-url` Secret keys. A release whose in-cluster
  Postgres predates #2049 holds the databases `chat` and `sched` and no
  `fleet`; the chart no longer supports it (reinstall it, or move its data by
  hand as below).
- `fleet backup`'s `-n public` fallback and `fleet restore`'s acceptance of a
  `public` dump.

**Decisions:**

- **The bare `DB_*` defaults** are now one database, user and database
  `fleet`/`fleet`, as bootstrap provisions. (Phase 1 kept the old per-store
  defaults, `chat`/`chat` and `fleet`/`sched`, for a box that might rely on
  them; none does.)
- **Old dumps are refused, not imported.** `fleet restore` of a dump whose
  store is in `public` (one taken before the box moved) refuses with a message
  saying so, before anything is dropped. Every box has fresh schema-mode
  backups; [BACKUP_RESTORE.md](BACKUP_RESTORE.md) documents the four commands
  that import an old dump by hand (restore into a scratch database, rename
  `public` to the store's schema, dump that schema, `fleet restore` it).
- **The `public` guard stays** (`dbschema.Ensure` and bootstrap's
  `fleet_db_store_readiness` refuse a database whose `public` holds a fleet
  store): it costs one catalog query and still catches `DATABASE_URL` pointed
  at an old database. A DSN without a store schema is still left in `public`
  rather than refused, because the stores' own internal tests still migrate
  their single-store templates (`internal/testdb`, M2) that way.
- **Test helpers:** M2's `testdb.LegacyPair` and `testdb.Reserve`, used only
  by the legacy-layout and consolidate tests, are removed; every other test
  already runs on the one-database template (`internal/testdb/fleetdb`).
- `scripts/e2e-boot-server.sh` boots the live e2e suite on one database,
  `fleet_e2e`, with `DATABASE_URL` alone.
- The per-store `--chat-database-url` / `--sched-database-url` flags and
  `ChatFrom`/`SchedFrom` were not merged into one flag: `DSNs` carries one
  `From` now, but the flags stay — each still names the store it addresses,
  and dropping them would break operator scripts for no gain.

**Moving data by hand** (a Helm evaluation release from before #2049, or any
other leftover two-database copy): for each store, restore or copy its
database into a scratch database, `ALTER SCHEMA public RENAME TO <store>`,
`pg_dump -Fc -n <store>`, and `fleet restore --db=<store>` the result into the
one database, as in [BACKUP_RESTORE.md](BACKUP_RESTORE.md).

## What deviated from the issue

- The schema is set through the DSN's `options` startup parameter rather than
  pgx `RuntimeParams`, so psql, pg_dump and the shell helpers get it from the
  same DSN.
- Phase 1 kept the bare `DB_*` defaults on the legacy pair and shipped
  `fleet db consolidate` (URL DSNs on one local server only) and
  `fleet db layout` for the move; phase 2 removed all three (above).
- There was never an in-cluster consolidation for the Helm chart's Postgres
  (an evaluation convenience): such a release is reinstalled or moved by
  hand.

## The rest of #2049 (follow-up PRs, in this order)

Phases 1 and 2 have shipped. The remaining phases ship as their own PRs under
the same issue, each updating this note:

- **M2 — template test databases: shipped.** Every DB test gets a fresh
  database cloned from a migrated template — the one-database layout, schemas
  `chat` and `sched` — and the DB test packages run in parallel in one
  `go test` ([TEST-DATABASES.md](TEST-DATABASES.md)). Phase 2 then removed
  its legacy-layout helpers (`testdb.LegacyPair`, `testdb.Reserve`).

**M4 — one identity table** ([IDENTITY.md](IDENTITY.md), ADR-0105): one
`core.users` table, in a third schema `core` the chat migrations create, and
one role vocabulary; the shadow sync and the identity seams are gone. The
chat store's backup carries schema `core` (`pg_dump -n chat -n core`).

**M5 — one scheduler** (ADR-0104): chat patrols and timers run on the sched
engine.

**M6 — one migration runner** ([MIGRATIONS.md](MIGRATIONS.md), ADR-0107): one
runner (`internal/migrations`), one directory (`internal/migrations/sql`, the
legacy files frozen under `legacy/chat` and `legacy/sched`) and one ledger,
`core.schema_versions`, which adopts the two per-store ledgers on the first
start.

## Text Postgres cannot store: content is cleaned, keys are not

Postgres refuses U+0000 in a text value (SQLSTATE 22021), and jsonb (not
json) also refuses the JSON sequence `\u0000` (22P05). fleet stores free-form
text it does not control — what a user types, what a model writes, a webhook
payload — so the store layers remove the character from the **content**
fields they write, through `internal/pgtext`: `pgtext.Content` for text and
`pgtext.JSON` for JSON text bound for jsonb (0x00 bytes first, then the
`\u0000` sequences; a doubled backslash followed by `u0000` is text and is
kept). The character carries no meaning in text.

| Store | Content fields cleaned |
|---|---|
| chat | the decoded message text the search index stores (`message_search_content`, written in the same transaction as a turn's history commit, a webhook's input commit and boot recovery), and the search query matched against it; conversation titles (create, auto-title, rename, branch, project chats, webhook chats); the input queue's message; memory content (personal, proposed, project); a project's name and instructions; a persona's display name, description, instructions and each patrol's title and prompt |
| scheduler | a task's title, prompt, description and tags (jsonb); a run's result, error message and structured output (jsonb); the run log (`logs.session_data`, jsonb); a trigger's prompt template; an inbound email's subject |

**Content is cleaned before it is checked.** A task's title, prompt and
description lose U+0000 in the normalisation each create and edit path runs
first: `validateTaskCreate` (every HTTP create, edit, batch, estimate and
import path), `storage.EnqueueTaskAs` (an approved chat `schedule_task`, the
`create_task` tool), `storage.UpdateEditableTask` (an approved `manage_tasks`
edit), a trigger run's rendered prompt (`triggerRunCreate`: a render of
nothing but 0x00 bytes falls back to the template's prompt) and a dead-letter
replay's replacement prompt; the CLI and bundle imports check emptiness on
the cleaned prompt. `models.ParseExecutionRequirements` itself reads a prompt
without U+0000, so every reader — the save-time checks, the serialization key
`NewTask` takes from the prompt, dispatch — agrees on the declaration even
when it is handed the prompt before it was cleaned. A chat message of nothing
but 0x00 bytes is refused as empty. A structured output is cleaned before it
is validated against its schema (`storage.validateStoredOutputContract`,
`UpdateTaskStatusAtomic`): removing the character afterwards could change
the object (keys `amount` and `amount\u0000` would collapse into one), so
what is validated is exactly what is stored.

**Keys are not cleaned.** Ids, emails, issuers, subjects, slugs, tokens,
idempotency keys and the names rows are looked up by — a task's name (the
import key), a project's MCP server names (catalog keys), a persona's name, an
input's client key, an inbound email's sender — go to Postgres without
U+0000 being removed (an email is trimmed and lowercased, `pgtext.EmailKey`).
A key holding U+0000 therefore fails its query: a lookup (an account by
email, a share token, a trigger slug, an OAuth state) matches no row, and a
write naming such a key fails.

**Identity values are refused where they enter**, so they get a clear
refusal instead of a failed query. Every email from outside goes through
`pgtext.CheckedEmail`, which checks the trimmed value **as sent** and only
then lowercases it. It refuses a Unicode control character (U+0000 among
them) or format character (U+200B, U+202E, U+FEFF, and U+200C/U+200D, which
IDNA2008 allows in some scripts' domain names), U+FFFD, text that is not
UTF-8, and a non-ASCII character whose lowercase form or case-fold orbit
holds an ASCII letter (U+212A KELVIN SIGN, U+017F LONG S, U+0130), which
lowercasing would map onto another account's ASCII address. Subjects,
issuers and tenants get the same check (`pgtext.ValidIdentity`). An email
that reaches a store key some other way and holds such a look-alike is only
trimmed, not lowercased (`pgtext.EmailKey`), so it matches no other account.
This is a deliberate trade-off for Turkish addresses: an email typed with an
uppercase İ (U+0130) is refused, so `İLKER@firma.com.tr` no longer signs in
as `ilker@firma.com.tr`; typed in lowercase, it does.

Entries written into a multiplayer participant or excluded list follow one
more rule, the list rule (`pgtext.ValidListEmail`): no backslash. The lists
are JSON text, and an entry with the characters `\u0000` would read there as
the sequence of a damaged list. Signing in and resolving a principal do not
refuse a backslash.

| Entry point | Refusal |
|---|---|
| OIDC ID token: email and subject claims (`webui.validateIDToken`) | sign-in refused; the reason is logged |
| Session cookies: `elcano_session` email, issuer, subject; `elcano_auth` email, tenant (`internal/websession`) | no session |
| Central Auth's signed account events: email, subject (`webui.verifySignedEvent`) | event refused |
| Password sign-in form email (`webui.handleLogin`) | the same answer as a wrong password |
| Any chat request's principal (`httpapi.authMiddleware`) and Operations Center session principal (`sched/handlers.userPrincipal`) | 400 `invalid user identity` |
| `principal.Resolve`: the email; an OIDC session's issuer and subject | not a member, with no lookup; session revoked, without the external-session lookup |
| Account creation: `store.CreateUser` (admin create), `identity.CreateAccount`, the admin-key `POST /users` of the Operations Center, `fleet sched user add`, `store.ImportUser` and a legacy bundle's scheduler users | input error; 400 from the routes |
| A legacy import's conversations and memories whose owner is refused | input error, reported by the import |
| Bootstrap admins (`FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS`): a refused entry is not lowercased, `identity.EnsureAdmin` refuses it | boot stops with a message naming the entry |
| `ADMIN_EMAILS` (`config.splitEmails`): a refused entry is kept as written | a warning at load; the entry makes no one an admin |
| An admin password reset's path email; U+0130 or U+212A in the path of `PATCH`/`DELETE /admin/users/{email}` | 400 |
| Entries written into a multiplayer chat's participant or excluded list (`participants`, `excluded`, `add` of `POST /conversations/{id}/multiplayer`), under the list rule | 400; every list in the request is checked before anything is written |
| A remote MCP server's share grantee; the target of a conversation or project transfer | the route's invalid-grantee / invalid-target answer |
| A user API key's account (`fleet sched apikey create --user`) | refused at creation |
| An inbound email's sender; an email trigger's approved-sender entry (normalised with `pgtext.EmailKey`) | matches no approved sender / approves no one |

`PATCH` and `DELETE /admin/users/{email}` and a multiplayer `remove`
normalise their value (trim, lowercase) and then match it exactly, so an
account or a participant entry stored before these checks — with a control
character, or with U+017F, which is already lowercase — can still be changed
or removed. A multiplayer `remove` writes an exclusion only for a value the
list rule admits; any other value is only deleted from the lists. The admin routes compare the target with
the signed-in admin by the same normalised form, so a look-alike cannot slip
past the guards against deleting or demoting yourself.

**Two emails name the same account exactly when their store keys are equal**
(`pgtext.SameEmail`: `pgtext.EmailKey` of each side). The owner, sender,
viewer and participant checks use it — the chat and its turn stream, history
and live events, workspace files, approvals (an email to the owner alone),
patrols, projects, conversation transfer, the Autonomous auto-submit sweep,
the remote MCP OAuth user match and the admin self-guards. The in-memory keys
an email is filed under (session approval rules, running approvals, a
multiplayer chat's allowed set, live stream viewers, a co-piloted browser
session's owner, participant workspace purge marks, rate-limit buckets) go
through `pgtext.EmailKey` too, as do the emails written out: session cookies,
admin audit rows, and the `active-participant` marker file a participant
turn leaves in the conversation's workspace stash.
An address in other capitals or with spaces around it (`Alice@Example.com`,
`ÉMILE@example.com`, a row an older release stored that way) is still the same
account. Two different lowercase addresses never are, even where Unicode case
folding calls their letters the same: `σ@x.com` and `ς@x.com`, `µ@x.com`
(MICRO SIGN) and `μ@x.com`, `ϑ`/`θ`, `ϰ`/`κ` and the other symbol forms, and
the Cyrillic small letters U+1C80–U+1C88 are each their own account, which
`strings.EqualFold` used to treat as one. A guard test
(`pgtext.TestIdentityEmailsAreNotCaseFolded`) fails on any new
`strings.EqualFold`/`bytes.EqualFold`, comparison of two `strings.ToLower`
results, or `strings.ToLower`/`ToUpper` of a value named like an email,
owner, sender, viewer or user, in production code unless it is listed with
the reason its values are not accounts (URL hosts, header and mode keywords,
connector names, team labels — a team restated in another case form, such as
`Πωλησεις` as `ΠΩΛΗΣΕΙΣ`, must not move or unshare anyone, so team labels
keep `strings.EqualFold`). The name check is a heuristic: it sees the usual
spellings (`userEmail`, `conv.UserEmail`, `entry.steer.user`, `user_name`),
not a value called `r`.

What this does not do:

- `messages.content` is `TEXT` holding JSON, so a NUL in a chat message is
  stored there as the JSON sequence `\u0000`, which text accepts; reloaded
  history still carries U+0000. Only the decoded copy the search index
  stores loses it.
- A `\u0000` sequence stored in a `TEXT` column holding JSON still breaks a
  `::jsonb` cast over that row (22P05), and Postgres does not promise to
  evaluate a row's other filters first. The casts over such columns are
  guarded: the message-content cast in `CountUserMessagesAfterResolution`
  only casts a row without the sequence (a row with it falls back to the
  timestamp boundary), and the multiplayer participant and excluded lists
  stored with it before entries were checked are read the way that never
  grants access — a participant list names nobody, an exclusion list excludes
  everybody — by the shared-chat listing and search. Account deletion still
  removes the deleted account from such a list, decoding it in Go. The lists
  themselves are not rewritten.
- Some SQL filters still lowercase an email themselves
  (`lower(user_email) = lower($n)`: the approval and resume queries, the
  admin chat and patrol listings, account deletion's chat orders), and their
  Go side passes the value trimmed, not through `pgtext.EmailKey`.
  Lowercasing never merges two different lowercase addresses, and for every
  address that can sign in or is stored it gives the same key as
  `pgtext.EmailKey`; the two differ only for a value holding a look-alike of
  an ASCII letter and a capital (U+0130 and U+212A are capitals themselves),
  which sign-in refuses and no stored email holds. The one value that can
  hold one is an admin's own filter text in the admin chat and patrol
  listings, which list every chat to that admin anyway.
- Web pages compare emails for display only (which chat is "mine"); the
  server decides access.
- Free-form fields not in the content table are stored as given and fail on
  U+0000, for example: the turn journal's tool input (kept byte for byte as
  the record of what ran), a chat patrol's own title and prompt, a task's
  output schema, a paused task's question and answer (`pending_question`,
  `pending_answer`), a run's error analysis (`error_analysis`), task memory
  values, prompt library content, user skill bodies, notification settings.
- A task's owner set by name is a lookup: a name holding U+0000 or a
  look-alike matches no user.
