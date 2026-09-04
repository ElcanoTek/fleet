# Shared files: the cross-chat file library

Admins publish files once — historical datasets, reference documents, price
lists — and **every conversation's agent can read them**, on both sandbox
backends, without re-attaching anything per chat. This is the *native*,
out-of-the-box way to give agents standing data; Dropbox/S3/Drive via an MCP
connector remains the right tool when the data already lives there.

```
admin uploads (Settings → Shared files, or POST /shared-files)
  → canonical bytes: <DataDir>/shared_files/<id>        (control-plane state)
  → manifest row:    shared_files table                  (name, folder, size, sha256)
  → staged copy:     <WorkspaceRoot>/shared/[folder/]name  (what sandboxes read)
  → every chat turn: a "Shared file library" prompt block lists shared/<…> paths
```

## Why two trees

The staged tree lives under the **workspace root** because that is the one
directory visible inside sandboxes on *both* backends — the podman bind mount
and the kubernetes workspace claim. (The chat-attachment uploads root is
host-only state that no sandbox mounts on either backend; chat attachments
solve the same reachability problem the per-conversation way, by being copied
into the sending conversation's own workspace directory at send time — see
[ATTACHMENT-SCOPING.md](ATTACHMENT-SCOPING.md) and
[ADR-0058](adr/0058-per-conversation-attachment-scoping.md).) But the workspace
mount is read-write, so the staged tree alone could be tampered with by a turn.
Hence:

- **Canonical bytes** stay in `<DataDir>/shared_files/<id>` — never mounted
  into any sandbox, so no agent can corrupt what the admin uploaded. Downloads
  and re-staging always come from here.
- **The staged tree is mounted read-only over the read-write workspace
  mount**: podman adds a nested `--volume <root>/shared:<root>/shared:ro`
  overlay; kubernetes re-mounts the workspace claim's `shared` subPath
  read-only into every sandbox pod. `write_file`/`edit_file` refuse the tree
  one layer earlier (it is registered as a read-only supporting-doc root), and
  `bash`/`run_python` writes fail on the mount itself.
- **A reconciler makes the staged tree converge to the manifest** — at boot,
  after every mutation, and on the hourly maintenance pass: missing or
  wrong-sized files are re-staged from the canonical bytes, strays are
  removed. A wiped workspace volume heals itself at the next boot or pass.

## How agents see it

Each conversation workspace gets a `shared` symlink (planted next to the
existing `protocols`/`personas`/… links) pointing at the staged tree, so
`shared/<folder>/<name>` resolves from the chat's cwd in `bash`, `run_python`,
and the file tools alike — same-path mounting makes the absolute target valid
inside the sandbox on both backends. Every chat turn appends a **Shared file
library** block (capped at 50 entries, like the workspace inventory block)
listing each file's `shared/…` path, size, and admin-written description, with
the instruction to copy a file into the workspace before modifying it.

Scheduled runs under `fleet serve` get the same announcement (#1301): the
block is appended once to the run's system prompt — computed at run start, so
the prompt stays byte-stable across the run's turns
(docs/PROMPT-CACHE-CONTRACT.md) — through the same renderer chat uses
(`sharedfiles.PromptBlock`), and the workspace seeding (#1290) plants the same
`shared` symlink, so `shared/<folder>/<name>` resolves identically. One-shot
`fleet task run` is the exception: it has no DB and therefore no library — no
announcement, and referencing `shared/` there resolves only if the operator
staged something at that path themselves. (Recurring-task *input* files
remain a separate mechanism: `tasks.files` staged per run into the MCP
workspace `inputs/` dir.)

## API (chat server)

- `GET /shared-files` — any member. `{files, total_bytes, max_total_bytes}`.
  Listing is member-level on purpose: the files are already readable from
  every chat, so hiding the catalog would be security theater.
- `POST /shared-files` — admin. Multipart like `/attachments` (repeated
  `files` field), plus optional `folder` (one sanitized path segment) and
  `description` applied to each file. Per-file cap: `FLEET_UPLOAD_MAX_BYTES`.
- `PATCH /shared-files/{id}` — admin. `{name?, folder?, description?}` —
  rename/move is metadata plus a re-stage; the canonical bytes never move.
- `DELETE /shared-files/{id}` — admin. Removes row, staged copy, canonical bytes.
- `GET /shared-files/{id}/download` — any member; streams the canonical bytes.

Admin here means the same rule as every `/admin/*` endpoint (`ADMIN_EMAILS`
allowlist OR `users.role = 'admin'`); viewers are read-only via the standard
role gate. `(folder, name)` is unique — a collision is a `409`, not a silent
rename.

**Two rows can collide in two different ways, and only one of them is that
unique constraint.** `(folder, name)` being unique still admits a root-level
file named `q3` alongside a folder named `q3`: different pairs, same staged
path. A filesystem cannot hold both, so the reconciler could never converge —
`Stage` failed in one direction with `not a directory` and in the other with
`file exists`, every pass returned an error forever, and which of the two files
the sandbox could see depended on map iteration order. Both write paths refuse
the second claimant with `ErrSharedFileNameIsFolder` → `409`, because the
request is the last moment a human is present to be told. Note what this does
**not** do: a deployment that already carries such a pair is not migrated, and
its `Sync` keeps erroring until an operator renames one side. Nor is the guard
race-free — "name must not equal any folder" is not expressible as a unique
index, so it is check-then-write under the handler mutex. That closes the
window within one process; two replicas racing the same pair can still land it.

## Governance: the size cap

`shared_files_max_total_mb` (admin settings registry; env default
`FLEET_SHARED_FILES_MAX_TOTAL_MB`, 10 GiB) caps the **library total**; an
upload that would cross it is refused with `413` before any byte lands. `0`
means unlimited, for deployments that genuinely want a huge library and accept
the cost — but note the honest physics: every byte exists **twice** (canonical
+ staged), so a 100 GB library consumes ~200 GB across the data dir and the
workspace volume (two PVCs on kubernetes). The default is deliberately modest;
this is a library, not a data lake — past a few GB, an object-store MCP
connector is the better tool.

## Kubernetes notes

- The staged tree lives in the workspace RWX claim, so it reaches every
  sandbox pod with **no image rebuild, no ConfigMap projection, no per-pod
  copy** — the same no-push posture as ADR-0049.
- The control plane creates `<WorkspaceRoot>/shared` *before* the pod pool
  spawns: if the kubelet created the subPath directory first it would be
  root-owned and the control plane (uid 1000) could no longer stage into it.
- Downloads/uploads flow through the control plane (canonical bytes live on
  the data PVC), so the library works even though pods never see the data PVC.

## Web UI

**Settings → Shared files** — one page for both tiers: members get the
read-only library (list, sizes, descriptions, download); admins additionally
get multi-file upload with folder + description, inline rename/move/describe,
delete, and a usage meter against the cap.

## Honest scope (deferred)

- **One folder level.** `folder/name` is the whole hierarchy — organization,
  not a filesystem. Nested trees were deliberately cut.
- **No per-file ACLs.** The library is workspace-global by design (that is the
  feature); projects/teams wanting scoped sharing should use project
  conversations + attachments or an MCP connector.
- **No chat-composer browser.** The library is announced in the prompt block
  and managed in Settings; a composer-side picker is a follow-on.
- **No `fleet task run` announcement.** The one-shot harness has no DB, so it
  has no library manifest to announce (or stage). Scheduled runs under
  `fleet serve` DO get the announcement as of #1301 — the former "no task
  prompt block" deferral shipped.
- **No versioning/dedup.** Re-uploading a name in the same folder is a `409`;
  delete-then-upload is the update path. sha256 is recorded per file, so
  integrity is checkable, but two identical uploads store two copies.
- **Images are files here, not vision input.** A shared PNG is readable by
  tools; it does not flow into the model as multimodal input the way chat
  image attachments do.
- **Tamper self-heal, not tamper-proof host.** The read-only mounts make
  in-sandbox tampering impossible; host-side edits to the staged tree are
  repaired by size comparison on the next pass (full-hash verification per
  pass was rejected as real I/O on multi-GB libraries for no added in-sandbox
  guarantee).
