# Uploads & storage management

Design note for the upload-limit + storage-cleanup feature (2026-07). What
shipped, how the pieces fit, and what was deliberately deferred.

## Why

A user uploading a >250 MB file in the chat composer got a silent failure:
no client-side size check existed, so the file sat queued until Send, spent
a full upload round-trip, and came back as a raw 413 body rendered in tiny
inline text (or an opaque `400 parse multipart: unexpected EOF` when the
batch tripped the Next.js proxy buffer first). Meanwhile three magic
numbers (256 MiB chat plane, 250 MB orchestrator plane and web validation,
`"256mb"` in `next.config.ts`) enforced parity by hope alone, the
orchestrator's `temp_uploads` cleanup was dead code, and upload/workspace
disk usage was invisible to admins.

## The limit

- **One knob**: `FLEET_UPLOAD_MAX_BYTES` (default **1 GiB**, validated
  positive). Plumbed to both upload surfaces:
  - chat plane `POST /attachments` (`internal/httpapi/attachments.go`)
  - orchestrator `POST /upload` (`internal/sched/handlers/upload.go`, via
    `handlers.Config.UploadMaxBytes`)
- **Advertised to the browser** as `upload_max_bytes` in `GET
  /server-config`. The chat composer screens picked files against it
  (`web/src/app/lib/uploadLimits.ts`) and the orchestrator task modal's
  `FileUpload` / `validateFile` default to the same constant
  (`DEFAULT_UPLOAD_MAX_BYTES`).
- **Whole-request cap** is 2× the per-file limit on `/attachments`, matched
  by `experimental.proxyClientMaxBodySize: "2gb"` in `web/next.config.ts`.
  Raising `FLEET_UPLOAD_MAX_BYTES` past ~1 GiB requires raising the proxy
  cap too — the config comment says so at both ends.

## Failure honesty

- Oversize files are refused **at pick time** in the chat composer with a
  `role="alert"` banner naming the file and both sizes; in-cap files from
  the same multi-select still attach.
- Server-side, every size rejection is a **413 with readable copy** —
  per-file (`"big.zip" is 1.5 GB — over this server's 1.0 GB per-file
  upload limit`), combined-request (`attach fewer files at once`), and the
  orchestrator single-file path (previously a bare `400 File too large`).
- Batch atomicity: all sizes are validated **before** any file is written,
  so a rejected batch no longer strands earlier files on disk until the
  TTL sweep.
- The composer's disabled Send button now explains itself (tooltip +
  placeholder) when attachments are queued but the message box is empty,
  and a `role="status"` banner warns when >200 MB is queued ("sending may
  take a while").
- The orchestrator upload's `ParseMultipartForm` in-memory threshold
  dropped from the full cap to 32 MiB — a 1 GiB upload streams to temp
  files instead of buffering in RAM.

## Cleanup

- **Hourly disk sweep** (`cmd/fleet/main.go`, ctx-bound `safe.Recover`
  goroutine): `store.SweepAttachments` (attachment uploads past the
  conversation TTL) + `handlers.CleanupTempFiles` (orchestrator
  `temp_uploads`, which previously had **no caller**). The existing
  post-turn sweeps remain; the timer covers idle servers.
- **Admin storage panel** (Settings → Admin → Server): byte totals for
  attachment uploads / task uploads / workspaces, host-disk headroom, the
  ten largest conversation workspaces with title/owner/pinned context, and
  a warning banner when chat data exceeds half the disk.
- **"Clean up now"** (`POST /admin/storage/cleanup`): deletes unpinned
  conversations idle past an operator-chosen cutoff (minimum 1 day;
  **pinned, archived, shared, and project-bound chats are never touched**
  — the same exemptions as the TTL sweep, enforced in
  `store.DeleteUnpinnedOlderThan`), reaps their now-orphaned workspace
  dirs, and sweeps aged upload/temp files. Returns counts + bytes freed.
  The panel previews how many conversations a given cutoff would remove
  before the operator commits.

## Endpoints

| Endpoint | Method | Gate | Purpose |
|---|---|---|---|
| `/admin/storage` | GET | admin | tree byte accounting + largest workspaces + reclaimable counts |
| `/admin/storage/cleanup` | POST | admin | `{older_than_days, delete_conversations, sweep_files}` → counts + bytes freed |

Web proxies live under `web/src/app/api/admin/storage{,/cleanup}` with the
usual session + CSRF checks; the upstream admin middleware remains the
authorization boundary.

## Deliberately deferred

- **No per-user quotas** and no byte ledger — the GET walks the trees on
  request, which is fine at single-box scale.
- **No S3/object-store offload** — explicitly out of scope; this is
  local-disk management for the single-big-box posture.
- **The admin cleanup always hard-deletes** eligible conversations (the
  operator asked for space back); it does not honor
  `FLEET_CONVERSATION_SOFT_DELETE` tombstoning.
- **Upload limit is boot-time env, not an Admin Features setting** — the
  Next.js proxy body cap is baked at build/boot, so a live-applied DB
  override could silently disagree with the proxy; the settings registry's
  live-apply rule excludes it (see `docs/ADMIN-SETTINGS.md`).
