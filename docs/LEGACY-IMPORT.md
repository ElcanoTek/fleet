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
- fleet:     `fleet import <bundle.json> [--dry-run]`

Both exporters emit the same envelope with different sections populated: chat
fills `chat`, moc fills `sched`. `fleet import` routes each section to the
matching database (chat store / sched store) and ignores absent sections.

## Migration runbook (single box, both apps → one fleet)

```sh
# 1. Stop the legacy services so no writes race the export.
sudo systemctl stop chat.target moc.service

# 2. Export. Each command reads the app's own env for its DB DSN.
sudo -u chat sh -c 'cd /opt/chat && set -a && . ./.env.local && set +a && \
    ./bin/chat-admin export --out /opt/chat/data/chat-bundle.json'
sudo -u moc  sh -c 'cd /opt/moc  && set -a && . ./.env      && set +a && \
    ./moc -export-fleet /opt/moc/data/moc-bundle.json'

# 3. Import into fleet (dry-run first). DSNs come from FLEET_CHAT_DATABASE_URL /
#    FLEET_SCHED_DATABASE_URL (fleet's .env.local), or --chat-database-url /
#    --sched-database-url flags.
fleet import /opt/chat/data/chat-bundle.json --dry-run
fleet import /opt/chat/data/chat-bundle.json
fleet import /opt/moc/data/moc-bundle.json  --dry-run
fleet import /opt/moc/data/moc-bundle.json

# 4. Task attachment files (moc tasks[].files) reference paths under moc's
#    DATA_DIR; copy them into fleet's data dir if any task carries files —
#    the import summary prints the count of tasks with file references.

# 5. Restart fleet; verify. Users keep their old passwords (bcrypt hashes
#    migrate verbatim). moc API keys (file-based api_keys.json) are NOT
#    migrated — mint fresh keys with `fleet sched apikey create`.
```

Re-running an import is safe: every write is keyed on the source identity
(chat user email, conversation UUID, memory UUID, sched user username, task
UUID, log task UUID) and either skips or idempotently upserts — no duplicates.

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
        "priority": 5, "instruction_self_improve": false,
        "status": "pending|scheduled|success|error|cancelled",
        "created_at": "…", "started_at": "…", "completed_at": "…", // RFC3339
        "result": "…", "error_message": "…",            // optional, terminal tasks
        "agent_session_id": "…",                        // optional
        "scheduled_for": "…",                           // RFC3339, optional
        "recurrence": "0 9 * * 1-5",                    // 5-field cron, optional
        "timezone": "America/New_York",                 // IANA, empty → UTC
        "created_by": "<uuid>",                         // sched users id, optional
        "files": ["…"] }                                // optional
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
- **memories** — insert-if-absent by UUID; `learned_at` is backfilled from
  `created_at`, `kind` defaults to `fact` (matching migration 026's backfill).
  A `conversation_id` referencing a conversation that didn't make it over is
  nulled (it only matters for pending proposals).

sched section:

- **users** — matched by **username**: if the username already exists in fleet
  (e.g. the bootstrap admin), the existing account wins and imported tasks'
  `created_by` is remapped to its UUID; otherwise the user is inserted with its
  original UUID, hash, role, and scopes preserved.
- **tasks** — upserted by UUID (idempotent re-runs). Live tasks
  (`pending`/`scheduled`) get scheduling normalized: a recurring task whose
  `scheduled_for` is in the past (or absent) has its next run recomputed from
  the cron spec in the task's timezone; a one-shot with a past `scheduled_for`
  fires shortly after import (same behavior moc's own importer had). Terminal
  tasks (`success`/`error`/`cancelled`) are preserved verbatim as history.
  Unknown statuses or an unparseable cron on a live task fail that record
  (counted + reported), never the whole import. `--live-only` skips terminal
  tasks and logs entirely if you don't want history.
- **logs** — run-history transcripts, upserted by task UUID with the JSON
  payload passed through byte-for-byte.

What intentionally does NOT migrate: chat approvals / turn metrics / turn
events (ephemeral operational data), chat/moc session tokens (users just log
in again), moc nodes and per-task node targeting (fleet has no nodes — the
exporter drops `target_node_*` and reports how many tasks referenced them),
and moc's file-based API keys and audit log (mint fresh keys in fleet).
