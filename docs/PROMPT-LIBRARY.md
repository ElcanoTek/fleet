# Hybrid prompt library

Fleet exposes one prompt library in both interactive Chat and the Operations
Center task form. It deliberately combines two ownership models:

- **Git-backed workspace prompts** come from the active client bundle's
  `prompts/` directory. Fleet reads `.yaml`, `.yml`, `.md`, and `.txt` files
  live, preserves their exact contents, and presents them as read-only entries.
  Pulling the config repository updates the catalog without restarting Fleet.
- **Workspace prompts** are created in the UI and stored in the scheduler
  database. Their author chooses private (only the author) or workspace-shared
  (readable by authenticated workspace members). Only the author or an admin can
  edit or delete one.

The picker supports search, inserting an entry into the current chat/task draft,
creating a prompt from that draft, editing UI-owned entries, and exporting the
visible hybrid library as a versioned JSON backup. Export is intentionally a
plain file download so it can be placed in OneDrive, Dropbox, or any ordinary
backup folder without a vendor integration.

## Capturing a good chat as a prompt

Two entry points open the same review dialog, which distills the chat into a
self-contained draft (a host-side model call, `FLEET_LIBRARY_PROMPT_MODEL`)
that the user reviews and edits before it is saved through the ordinary
`POST /prompts` path. Nothing is stored until they save.

- **Under a finished assistant reply** — a "Save as prompt" action beside Copy
  and Branch. This is the primary affordance: the moment someone decides an
  interaction is worth keeping is the moment they finish reading a good answer,
  and until this shipped the only route was to leave the reply, find the chat's
  row in the sidebar, hover it, and open a kebab. The reply's persisted message
  id is sent as `up_to_message_id`, so the transcript fed to the synthesizer is
  cut there — a chat that carried on into an unrelated tangent does not fold
  the tangent into the saved recipe.
- **The conversation kebab's "Save to prompt library…"** — unchanged, and
  distills the whole chat. This is the right one for a long multi-step
  workflow whose value is the entire recipe.

`up_to_message_id` is advisory: it is optional, a malformed body is not an
error, and an id the server cannot find in the loaded history degrades to
distilling the whole conversation rather than to distilling nothing.

## Bundle format

Create `<bundle>/prompts/` and add prompt files. For YAML, top-level `name`,
`description`, and `goal` fields provide catalog metadata; the full YAML remains
the inserted prompt. For Markdown, the first level-one heading is the name and
the first prose line is the description. Otherwise Fleet derives the name from
the filename. `README.*`, symlinks, unsupported extensions, invalid UTF-8,
files over 256 KiB, and entries past the 256-file catalog limit are skipped.

```yaml
name: Weekly project brief
description: Summarize progress, risks, decisions, and next steps.
inputs:
  - project notes
  - issue tracker updates
instructions:
  - Cite the source for each claim.
  - Call out owners and due dates for every next step.
```

Git entries are trusted workspace content, like personas and protocols. They do
not grant tools or permissions: selecting one only fills the ordinary composer
or task prompt, and the existing create/run governance still applies.

## API

The authenticated Operations API exposes:

- `GET /prompts` — merged visible catalog.
- `POST /prompts` — create a private or workspace prompt.
- `PUT /prompts/{id}` / `DELETE /prompts/{id}` — owner/admin mutations for
  database-backed entries.
- `GET /prompts/export` — versioned JSON export.

Git entries have `source: "git"` and `read_only: true`; UI-owned entries have
`source: "workspace"`, their visibility, and an `owned_by_caller` affordance.

## Shipped scope and deliberate deferrals

- Shipped: live Git catalog, shared picker on both surfaces, private/shared UI
  storage, CRUD, search, exact-content insertion, JSON backup, and both capture
  entry points above.
- Deferred: writing back into Git (Fleet never mutates the external bundle),
  automatic cloud-drive sync, prompt version history, and JSON re-import. The
  exported format is versioned so import can be added compatibly later.
