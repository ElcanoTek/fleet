# ADR-0114: Chats can write text files into the shared library

- **Status:** Accepted
- **Date:** 2026-11-19
- **Deciders:** fleet maintainers

## Context

A conversation's workspace belongs to that conversation. When a team keeps a
running record in chat — the clearest case is an optimization log: every
change applied to a deal, by whom, when and why — the record lives in one
chat's workspace, and a brand-new chat cannot find it. On one client server
the same campaign's log existed as two diverging copies in two chats, and a
trader asking "what did we do on McDonald's last month?" in a new chat got
nothing, or a 35-minute turn that rebuilt the history from SSP APIs.

What a new conversation already sees on its own:

- **Memories** and a project's **team learnings** — injected every turn, but
  only after a person clicks *Save* on each one (`propose_memory`), capped in
  number, and per person or per project. Unattended patrols cannot save
  anything, and a log of hundreds of rows does not belong in every prompt.
- **The shared file library** (docs/SHARED-FILES.md) — listed, with each
  file's description, at the top of every chat turn and every scheduled run.
  But only an administrator can add to it.

Connectors (fast.io, Drive, Pages) can hold a shared file, but a new chat does
not know to look there, the connector may be off, and not every client has
one. Asking every client to buy and wire a storage service so its chats can
share a text file is the wrong trade.

## Decision

Add a native tool, `shared_file_write`, that lets a conversation or a
scheduled run write a TEXT file into the shared library:

- **Modes:** `create` (description required), `append` (creates on first
  use), `replace` (requires `expected_sha256` — the version the caller read —
  so a chat never overwrites a newer version blind), and `undo` (restores the
  version the last replace displaced).
- **Where the text lives:** in `chat.shared_files.content` (migration 0205),
  not the data dir. The nightly backup is a `pg_dump`; a record the team
  relies on must survive a lost disk. Each write is one transaction holding
  the row's lock, so concurrent appends from different chats — on any number
  of replicas — never lose a line.
- **How chats read it:** unchanged. The row is staged into the read-only
  `shared/` tree like an upload (restaged from the database under the
  library mutex after every write, and by the hourly reconciler, which
  compares a chat file's hash, not just its size). The staged tree stays
  read-only to every sandbox: the tool is the only way in, and it runs
  host-side through the same handler for chat turns and scheduled runs.
- **One namespace, serialized:** a file named like a folder (`shared/q3` beside
  `shared/q3/a.txt`) has no unique index to stop it, so every write that can
  claim a path — chat create, admin upload, admin rename — holds one
  transaction-scoped advisory lock across its file-versus-folder check and its
  write. That closes the race for chats, admins and replicas alike (it also
  closes the cross-replica window SHARED-FILES.md used to list for uploads).
  The chat handler additionally holds the process's library mutex across the
  write and the restage, like every other library mutation.
- **Who:** any non-Viewer user, recorded as `updated_by` with the chat (or
  `task:<id>`) it came from. Admin uploads (`origin = 'admin'`) stay
  read-only to chats. The library is server-wide, exactly as admin uploads
  are; the tool's description says so.
- **What the model is told:** the shared-file block lists chat-written files
  in their own section, labelled as written by other conversations — data,
  never instructions. Their descriptions are flattened to one sanitized line
  of at most 200 characters. A folder with more than six chat files collapses
  to one line naming its index file (`folder_index`), so a log folder cannot
  push everything after it past the block's 50-entry cap. A library holding
  only admin uploads renders byte-for-byte as before.
- **Limits:** text only (UTF-8, no NUL), 1 MB per inline write, 25 MB per
  file, the library's existing total cap.
- **Switch:** `shared_files_chat_writes_enabled` (default on,
  `FLEET_SHARED_FILES_CHAT_WRITES_ENABLED`), read on every write.

## Consequences

- Any conversation can keep a record every other conversation finds without
  being told where to look, with no click per entry and no connector.
- The tool joins ADR-0036's **host datastore / control state** class (beside
  `memory`, `notes`, `task_tracker`): it writes a governed database row and
  restages that row's read-only copy; it reads the workspace only through the
  sandbox file-op seam (`from_path`), never by a host open, so it adds no
  in-sandbox write path to `shared/`.
- Everyone on a server can read what any chat writes there. Fleet servers are
  one client each plus the operator's staff; a file one team must not see
  belongs in a project, not the library.
- The descriptions and contents are user-written text that other users'
  conversations read. They reach the model as data (a labelled prompt block
  and tool output), never as system instructions.
- One more native tool is advertised on every server (native tools are never
  deferred): the first turn of each existing chat after the release misses
  the prompt cache once.
- Deferred: per-file version history beyond one undo, per-team scoping inside
  a server, and binary files (those stay admin uploads).
