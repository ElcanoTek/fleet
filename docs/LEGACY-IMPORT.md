# Legacy import — migrating chat + moc data into fleet

Fleet replaces the standalone **chat** and **moc** deployments. This document
specifies the one-time migration path: each legacy app exports a
**fleet migration bundle** (a versioned JSON file), and `fleet import` ingests
it. The bundle format is fleet's own contract; all legacy-schema knowledge
(column mapping, status normalization, node-field dropping) lives in the
exporters inside the deprecated repos, NOT here. Fleet only knows how to
consume the bundle.

- chat repo: `chat-admin export` (operator wrapper: `chat export`)
- moc repo:  `moc -export-fleet <file>` (operator wrapper: `moc export`)
- fleet:     `fleet import <bundle.json> [--dry-run] [--live-only] [--overwrite]`

Both exporters emit the same envelope with different sections populated: chat
fills `chat`, moc fills `sched`. `fleet import` routes each section to the
matching database (chat store / sched store) and ignores absent sections.

## Migration runbook (single box, both apps → one fleet)

> **Cutting over a box that is live on the legacy stack?** This section covers
> the data moves only. The full operational sequence — backups, disabling (not
> just stopping) the legacy units, bootstrap's DB-collision guard, Caddy/port
> coexistence, re-pointing external intake callers, decommissioning — is
> [`docs/CUTOVER.md`](CUTOVER.md). Read it first.

```sh
# 1. Stop AND disable the legacy services: no writes racing the export, and no
#    reboot resurrecting them (all legacy units are WantedBy=multi-user.target)
#    to fight fleet over ports 8080/8000/3000 or double-fire migrated tasks.
sudo systemctl disable --now chat.target chat-server.service chat-web.service
sudo systemctl disable --now moc.service
#    (Worker boxes: also disable the legacy gig runner daemon there.)

# 2. Export. Each command reads the app's own env for its DB DSN.
sudo -u chat sh -c 'cd /opt/chat && set -a && . ./.env.local && set +a && \
    ./bin/chat-admin export --out /opt/chat/data/chat-bundle.json'
sudo -u moc  sh -c 'cd /opt/moc  && set -a && . ./.env      && set +a && \
    ./moc -export-fleet /opt/moc/data/moc-bundle.json'

# 3. Import into fleet (dry-run first). DSNs come from FLEET_CHAT_DATABASE_URL /
#    FLEET_SCHED_DATABASE_URL, or --chat-database-url / --sched-database-url
#    flags. On a systemd deploy the DSNs live in /etc/fleet/fleet.env (0600,
#    root-only) — NOT a .env.local — so source it explicitly:
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/chat/data/chat-bundle.json --dry-run'
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/chat/data/chat-bundle.json'
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/moc/data/moc-bundle.json --dry-run'
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/moc/data/moc-bundle.json'
#    (A dev deploy with the DSNs in .env.local can call `fleet import` directly.)

# 4. Non-DB state — the bundles carry database rows only:
#    a. moc task attachment files (tasks[].files) live under moc's DATA_DIR
#       (<data>/temp_uploads); copy them into fleet's data dir
#       (/var/lib/fleet/data/temp_uploads on a systemd deploy) if any task
#       carries files — the import summary prints the count.
#    b. the legacy chat data dir (email attachments + uploaded images):
#       migrated message history references these by their ABSOLUTE paths on
#       the legacy box (e.g. /opt/chat/data/attachments/...). fleet re-reads
#       those exact paths on conversation replay and silently drops missing/
#       unreadable files — so leave the files at their original path and make
#       them readable by the fleet service user (e.g.
#       `chown -R fleet:fleet /opt/chat/data`). See CUTOVER.md step 6.

# 5. Restart fleet; verify. Users keep their old passwords (bcrypt hashes
#    migrate verbatim). moc API keys (file-based api_keys.json) are NOT
#    migrated — mint fresh keys with `fleet sched apikey create`.
```

Re-running an import is safe: every write is keyed on the source identity
(chat user email, conversation UUID, memory UUID, sched user username, task
UUID, log task UUID) and any identity already present in fleet is **skipped**
— no duplicates, and nothing fleet has since written is reverted. In
particular, a sched task that already ran in fleet keeps its terminal
status/result (it will not flip back to pending and run again), a leased task
keeps its lease, and fleet-side run history is never replaced. If you
genuinely want the bundle's snapshot to win — restoring a wiped database from
a bundle — pass `--overwrite`, which replaces already-present sched tasks and
run logs in place.

Both DSN resolvers fall back to the generic `DATABASE_URL`. Because the chat
and sched stores must live in **distinct** databases (fleet serve refuses to
start otherwise), `fleet import` refuses to write a section whose DSN reached
that shared fallback while the other store's DSN resolves to the same
database — set `FLEET_CHAT_DATABASE_URL` and `FLEET_SCHED_DATABASE_URL` (or
pass the `--*-database-url` flags) so each section lands in its own DB. This
applies to single-section bundles too: the runbook's two imports would
otherwise mix both schemas into one `DATABASE_URL` database.

## Bundle format: `fleet-migration-bundle` version 1

```jsonc
{
  "format": "fleet-migration-bundle",   // required, exact string
  "version": 1,                          // required
  "exported_at": "2026-07-04T12:00:00Z", // RFC3339, informational
  "source": "chat v1.2.3",               // freeform tool id, informational

  // ── chat section (optional) — timestamps are UNIX SECONDS (int64),
  //    matching the chat store's native representation on both sides.
  "chat": {
    "users": [
      { "email": "a@b.c", "password_hash": "$2a$...", // bcrypt, verbatim
        "created_at": 1720000000, "updated_at": 1720000000 }
    ],
    "conversations": [
      { "id": "<uuid>", "user_email": "a@b.c", "title": "…",
        "persona": "victoria", "model": "", "pinned": false, "lockdown": false,
        "optional_mcp_servers_enabled": ["…"],          // optional
        "created_at": 1720000000, "updated_at": 1720000000,
        "messages": [
          { "role": "user|assistant|tool", "type": "text|reasoning|tool_call|tool_result|summary|turn_summary",
            "content": { }, // raw JSON exactly as stored in messages.content
            "created_at": 1720000000 }
        ] }
    ],
    "memories": [
      { "id": "<uuid>", "user_email": "a@b.c", "content": "…",
        "source": "manual|chat|proposed", "conversation_id": "<uuid|omitted>",
        "created_at": 1720000000, "updated_at": 1720000000 }
    ]
  },

  // ── sched section (optional) — timestamps are RFC3339 strings,
  //    matching the sched store's TIMESTAMPTZ columns.
  "sched": {
    "users": [
      { "id": "<uuid>", "username": "brad", "password_hash": "$2a$...",
        "role": "admin|client|readonly", "scopes": [],
        "created_at": "2026-01-01T00:00:00Z",
        "last_login": "2026-06-01T00:00:00Z" }          // optional
    ],
    "tasks": [
      { "id": "<uuid>", "prompt": "…",                  // prompt required
        "name": "", "description": "",                  // optional
        "model": "slug", "fallback_model": "slug",      // optional
        "max_iterations": 30,                           // optional
        // priority uses FLEET's scale: 0–100, LOWER = more urgent, 0 = unset
        // (normalized to Normal/50 on import). The legacy scheduler's scale
        // was higher-is-more-urgent — the exporter owns that mapping.
        "priority": 5, "instruction_self_improve": false,
        "status": "pending|scheduled|success|error|cancelled|dead_lettered",
        "created_at": "…", "started_at": "…", "completed_at": "…", // RFC3339
        "result": "…", "error_message": "…",            // optional, terminal tasks
        "agent_session_id": "…",                        // optional
        "scheduled_for": "…",                           // RFC3339, optional
        "recurrence": "0 9 * * 1-5",                    // 5-field cron, optional
        "timezone": "America/New_York",                 // IANA, empty → UTC
        "created_by": "<uuid>",                         // sched users id, optional
        "serialization_key": "client:acme",             // optional, opaque (#709)
        "files": ["…"],                                 // optional
        "file_names": ["…"] }                           // optional, pairs 1:1 with files
    ],
    "logs": [
      { "task_id": "<uuid>", "session_data": { } }      // raw JSON, verbatim
    ]
  }
}
```

## Import semantics

`fleet import` validates the envelope (`format`, `version`), then applies each
section. `--dry-run` runs the full validation + planning pass and prints the
plan without writing.

chat section:

- **users** — inserted with hash + timestamps preserved; existing email → skipped
  (never overwrites a fleet-side password). Role defaults to `member`; promote
  admins afterwards with `fleet chat user role`.
- **conversations** — a conversation UUID that already exists is skipped whole
  (messages included), which is what makes re-runs safe. Otherwise the
  conversation row and all messages are written in one transaction, preserving
  ids and timestamps. After the section is applied, the FTS side-table is
  backfilled (`BackfillSearchContent`) so migrated history is searchable.
  Import warns (never fails) when a conversation's
  `optional_mcp_servers_enabled` entry or non-empty `persona` doesn't exist in
  the loaded client-config bundle: an unknown server opt-in is silently inert
  (legacy chat stored un-suffixed names; fleet bundles typically use `*_mcp`),
  and an unknown persona hard-errors every turn on that conversation until it
  is reassigned.
- **memories** — insert-if-absent by UUID; `learned_at` is backfilled from
  `created_at`, `kind` defaults to `fact` (matching migration 026's backfill).
  A `conversation_id` referencing a conversation that didn't make it over is
  nulled (it only matters for pending proposals).

sched section:

- **users** — matched by **username**: if the username already exists in fleet
  (e.g. the bootstrap admin), the existing account wins and imported tasks'
  `created_by` is remapped to its UUID; otherwise the user is inserted with its
  original UUID, hash, role, and scopes preserved.
- **tasks** — inserted by UUID; a UUID already present in fleet is skipped so
  a re-run never reverts state fleet has since written (`--overwrite` replaces
  it instead — restore mode). Live tasks (`pending`/`scheduled`) get
  scheduling normalized: a recurring task whose `scheduled_for` is in the past
  (or absent) has its next run recomputed from the cron spec in the task's
  timezone; a one-shot with a past `scheduled_for` fires shortly after import
  (same behavior moc's own importer had). A `scheduled` task with no
  recurrence and no `scheduled_for` is kept but warned about — it never fires
  until rescheduled. Terminal tasks (`success`/`error`/`cancelled`/
  `dead_lettered`) are preserved verbatim as history. Unknown statuses or an
  unparseable cron on a live task fail that record (counted + reported),
  never the whole import. `--live-only` skips terminal tasks and logs
  entirely if you don't want history. The optional `serialization_key`
  travels verbatim, so a live recurring task keeps its ≤1-active-per-key
  mutual exclusion in fleet's scheduler (#709).

  **Runtime defaults change on migrated live tasks:** fleet mints imported
  tasks with `allow_network=false` (the execution sandbox is sealed with
  `--network=none`) and no MCP server selection, whereas v1 runs had full
  egress and prompt-driven MCP loading. A live recurring task that needs
  network or connectors WILL fail or degrade on its first post-cutover run —
  the import summary counts these tasks; re-scope them (task edit:
  `allow_network`, `mcp_selection`) before their next fire.
- **logs** — run-history transcripts, keyed by task UUID with the JSON
  payload passed through byte-for-byte. A task UUID that already has a log in
  fleet is skipped (`--overwrite` replaces it); a log whose task exists
  neither in the bundle nor in fleet fails that record (it would violate the
  logs→tasks foreign key), in dry-run and real runs alike.

What intentionally does NOT migrate: chat approvals / turn metrics / turn
events (ephemeral operational data), chat/moc session tokens (users just log
in again), moc nodes and per-task node targeting (fleet has no nodes — the
exporter drops `target_node_*` and reports how many tasks referenced them),
and moc's file-based API keys and audit log (mint fresh keys in fleet).
