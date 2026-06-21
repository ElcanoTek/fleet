# Fast.io MCP Protocol

**When this applies:** Any task that reads, writes, or shares files on fast.io — i.e. any step that calls `fastio_find`, `fastio_upload_file`, `download_url`, or an `mcp_fast_io_*` tool.

Fast.io is a workspace platform for agent-to-human handoff. Cutlass exposes three native tools — `fastio_find` (file discovery), `fastio_upload_file` (path-based uploads with bytes off context), and `download_url` (one-call URL→disk fetcher used after `mcp_fast_io_download action=file-url`) — alongside the seven `mcp_fast_io_*` tools (`storage`, `download`, `upload`, `workspace`, `share`, `worklog`, `comment`) that cover the download → update → upload → share → log → tag pattern. The platform's audit trail is how humans know what the agent did; without `worklog` entries the work is effectively invisible.

## The default discovery path: `fastio_find` (native)

When a task says *find*, *locate*, or *look up* a file on fast.io — or when you need to find one before downloading/amending it — **call `fastio_find`**. It is a thin wrapper around `mcp_fast_io_storage action=search` + `action=details` that fixes three sharp edges of the raw flow:

1. **ELC-code auto-promotion.** Fast.io's keyword search is AND-tokenized — every word must appear in the indexed text. A natural phrase like `ABC plumbing` against a workspace where reports are named `ABC_ELC00109_Overall_Report.csv` returns the *wrong* file and silently misses the reports. `fastio_find` detects any `ELCxxxxx` code in the query and runs an additional ELC-only search; results are unioned.
2. **One discovery turn instead of nine.** The raw flow tends to spiral: search → list-folder → multiple `details` calls to disambiguate near-identical names. `fastio_find` compresses the whole loop into 1 + N fallback searches plus one bulk-details call, with response capped well under any context-bloat threshold.
3. **Same-name duplicates surfaced by design.** Variants of the same report often live in different folders. `fastio_find` lists every variant sorted newest-first.

```
fastio_find
  query="ELC00109"                       # or "ABC plumbing ELC00109" — ELC code auto-promoted
  workspace_id="***REMOVED***"     # from mcp_fast_io_workspace action=list; required
  limit=10                               # optional; 1-25, default 10
```

Returns a single tight markdown table — `id | name | parent | modified | size | mimetype` — sorted newest-first.

**File-pick policy (cutlass one-shot):**
- Exactly 1 match → proceed.
- 2+ matches → if your task instructions disambiguate (date, parent folder, exact filename), use the row that matches; otherwise pick the newest and **surface ALL matches in your final report** so the operator can audit. If the run is genuinely ambiguous, `COMPLETE_WITH_FLAGS` rather than guessing — drafting against the wrong file is worse than asking on the next run. There is no user to interrupt mid-task.

**Use `fastio_find` for any file-discovery question.** Reach for `mcp_fast_io_storage action=search` directly only when you need a parameter `fastio_find` does not expose (cursor pagination past the first 25 hits; semantic/RAG mode via `intelligence=true`, which we do not enable by default — see the non-negotiables below).

## The default upload path: `fastio_upload_file` (native)

When you've just produced a file locally (via `write_file`, `run_python` with openpyxl/reportlab/python-docx, etc.) and need it in fast.io, **this is the tool**. It takes a path, not bytes — cutlass reads the file from disk, base64-encodes it server-side in Go (deterministic; no length-mismatch validator failures), and forwards it via the underlying `mcp_fast_io_upload` stream-upload action. You never see the bytes.

```
fastio_upload_file
  path="report.xlsx"                  # workspace-relative or absolute; required
  workspace_id="***REMOVED***"  # from mcp_fast_io_workspace action=list; required
  filename="Master Tracking.xlsx"     # optional; defaults to basename(path)
  parent_node_id="root"               # optional; defaults to root
  content_type="application/..."      # optional; auto-detected from extension
```

Returns the same fast.io response shape as the raw upload (`node_id`, `web_url`, `_next`). 5 MB raw cap — files larger than that need the chunked blob flow below. **Per the audit-trail rule below, follow every successful upload with `mcp_fast_io_worklog append`.**

**Do NOT** try to do the upload yourself via `run_python` + `mcp_fast_io_upload action=stream-upload` with inline `content_base64`. Cutlass actively rejects that call at the dispatcher (see the `_recovery` block of the error if you hit the guard). Pass the path through `fastio_upload_file` instead.

## Enabled tools (only these are available)

| Tool | Use for |
|------|---------|
| `mcp_fast_io_workspace` | Pick/list workspaces. Actions: `list`, `available`, `details`. **MUST NOT** call `enable-workflow`, `metadata-*`, `create-note`, or quickshare actions unless the user asks for them. |
| `mcp_fast_io_storage`   | Find, list, move, rename, search files. Actions: `list`, `details`, `search`, `create-folder`, `rename`, `move`, `copy`, `version-list`, `version-restore`. **For finding files, prefer the native `fastio_find` (see above)** — it handles ELC-code promotion, same-name duplicates, and bulk hydration in one call. |
| `mcp_fast_io_download`  | Pre-authenticated download URLs. Actions: `file-url`, `zip-url`. Pair with the native `download_url` tool to land the bytes on disk in one call. |
| `mcp_fast_io_upload`    | Chunked upload. Actions: `create-session`, `chunk`, `finalize`, `web-import`, `status`. |
| `mcp_fast_io_share`     | Branded delivery (`send`) or collection (`receive`) links. Actions: `create`, `update`, `details`, `list`. |
| `mcp_fast_io_worklog`   | **Mandatory audit trail.** Actions: `append`, `list`. Profile/workspace-level chronological log. |
| `mcp_fast_io_comment`   | File-anchored feedback and `@`-mentions. Actions: `add`, `list`, `reaction-add`. Use to tag specific humans on specific files. |

Do **NOT** assume `auth`, `task`, `approval`, `todo`, `ai`, `org`, `member`, `invitation`, or `asset` tools are available — they are intentionally not registered. If a job seems to need one of them, surface the gap and proceed without it; do not simulate it.

## Non-negotiables

1. **MUST `worklog append` after every state-changing fast.io action.** Triggers: file uploaded, file overwritten, folder created, file moved/renamed, share created/updated, share archived. Content must answer "what changed, why, where." One batched entry after a group of related actions is fine — one entry per session is not.
2. **MUST NOT delete-then-reupload to edit a file.** Same-name upload into the same parent folder **overwrites in place** (preserves `node_id`, old content becomes a recoverable version). Delete+reupload breaks references from existing shares, worklogs, and links.
3. **MUST locate files via `fastio_find` before uploading** when the task names a specific file. Do not create a parallel copy because the original was not found on the first try — retry with a different query (drop natural-language tokens; use the ELC code alone), widen the scope, or flag the run.
4. **MUST NOT enable workspace intelligence** (`intelligence=true`) unless the user explicitly asks for RAG or semantic search. It costs 10 credits/page on ingest and is non-refundable. The default (`false`) is correct for every storage/share/update workflow.
5. **MUST prefer `upload web-import` when the source is a URL** (HTTP/HTTPS, Google Drive, OneDrive, Dropbox). Single call, no chunking.

## Worklog vs. comment — when to use which

Worklog and comment are complementary, not interchangeable. They usually appear together on state-changing work.

| | `worklog append` | `comment add` |
|---|---|---|
| **Scope** | Profile-wide (workspace or share) | Anchored to one file (and optionally a region, page, timestamp, or text selection) |
| **Audience** | Anyone reviewing the workspace's activity feed | Anyone looking at that specific file |
| **Required?** | **MUST** after every state-changing action | **SHOULD** when file-level context matters or a specific human needs to be notified |
| **Mentions?** | No | Yes — `@[user:<opaqueId>:Name]`, `@[profile:<id>]`, `@[file:<fileId>:name.ext]` |
| **Typical content** | "Updated `X.xlsx`: added 2 rows for 2026-04-15 (Display + OLV channels). Source: DSP export email received 2026-04-16." | "Row added here because DSP schema changed on 2026-04-14. @[user:abc123:Brad] FYI." anchored to row 47 |

**Rule of thumb:**
- Every state change → **worklog entry** (mandatory audit trail).
- The change has *file-specific* context a reader would miss without it, or a named person must see it → **also** add a comment on that file.
- Short-form acknowledgment on an existing comment → `reaction-add`, not a new comment.

Do not open multiple comments on the same file for a single batch of changes — one summary comment with the full context is better than N noisy ones.

## Core workflows

### Download → update → upload (the workbook refresh pattern)

```
workspace list                        → pick workspace_id
fastio_find query="<name or ELC>"     → find node_id (and parent); newest row
  workspace_id=<id>                     is usually canonical when there are
                                        duplicates (see file-pick policy above)
download file-url node_id=<id>        → returns a short-lived signed URL
download_url url=<signed url>         → lands the bytes on disk in one call;
                                        defaults output_dir to cwd. Two-call
                                        run_python(requests.get) still works.
<local edit — Python, openpyxl, etc.>
fastio_upload_file path=<local file>  → same filename + parent_node_id as the
  workspace_id=<id>                     original; cutlass handles base64 in Go
  filename=<original filename>          and bytes stay out of your context.
  parent_node_id=<original parent>      Returns node_id (unchanged — overwrites
                                        in place) and web_url.
worklog append entity_type=profile
  content="Updated <filename>: added <N> rows for <date range>. Source: <where>."
```

The upload step **overwrites the existing node in place** when `filename` + `parent_node_id` match. Do not delete the old node first.

### Binary payload over 5 MB: chunked blob flow

`fastio_upload_file` caps single-call uploads at 5 MB raw (to fit safely under the typical 10 MB MCP message limit after base64 expansion). For files larger than that, drive the chunked blob flow yourself via the raw `mcp_fast_io_upload` tool:

```
mcp_fast_io_upload action=create-session profile_type=workspace
  profile_id=<workspace_id> parent_node_id=<folder_id> filename=<name>
  → response includes session_id AND a ready-to-paste POST /blob curl
POST the file bytes to the blob URL (run_python with requests, or bash curl)
  → response includes blob_id
mcp_fast_io_upload action=chunk session_id=<id> blob_id=<blob_id>
mcp_fast_io_upload action=finalize session_id=<id>
  → response has the new node_id and web_url
```

**Do not** call `mcp_fast_io_upload action=stream-upload` (or `batch`) with inline `content_base64` from your own context — cutlass rejects oversized inline uploads at the dispatcher with a hint pointing back here. The two failure modes that motivated the rejection:
- Emitting a 50 KB base64 string costs thousands of completion tokens and tends to be mangled (truncated, re-escaped, or near-verbatim-but-not-identical) before it leaves your context — fast.io's `len % 4 == 0` validator then rejects the upload with an opaque "invalid length" error.
- There is no `${tool:call_id.vars.name}` substitution. Inlining a value from `run_python.return_vars` requires you to copy the literal string into the next call's arguments verbatim, which is exactly what `fastio_upload_file` (for ≤5 MB) and the blob flow (for >5 MB) let you skip.

### Deliver a file to a human

```
share create workspace_id=<id> type=send storage_mode=workspace_folder folder_node_id=<id>
share update share_id=<id> password=... expires=... (as requested)
worklog append content="Created share <custom_name> for <recipient>. Link: <web_url>."
```

Return the `web_url` from the response to the user — it is the branded page the recipient opens. Never hand back raw download URLs when a share was requested.

### Collect files from a human

`share create ... type=receive`. For anonymous drops (no account required) pass `anonymous_uploads_enabled=true` with an "Anyone" access level. Log the share URL via `worklog append`.

## Handoff checklist (before reporting completion)

Before finishing any task that touched fast.io, verify:

- [ ] Every state-changing action has a corresponding `worklog append` entry.
- [ ] The final worklog entry names the artifact (filename or share custom_name), the change (added/updated/created/shared), and the outcome (success, flags, recipient).
- [ ] If a specific human needs to act on a file change, a `comment add` with an `@[user:...]` mention was posted on that file.
- [ ] All URLs returned to the user are `web_url` values from tool responses — never constructed manually.
- [ ] If the workbook/file was updated in place, `storage version-list` confirms the new version exists.

If any box is unchecked, append the missing worklog entry (or comment) before emitting the final report. A completed task without a worklog trail is **COMPLETE_WITH_FLAGS**, not **COMPLETE**.

## Gotchas

- **Profile IDs** are 19-digit numeric strings. **Node IDs** are 30-char alphanumeric opaque IDs (shown with hyphens). Don't swap them.
- **`profile_type`/`profile_id`** are the canonical parameter names; `context_type`/`context_id` are accepted aliases. Pick one and stay consistent within a workflow.
- **Response `_next` hints are suggestions, not orders.** Read them — they usually point at `worklog append` after state-changing actions, and skipping that is what produces silent-agent runs.
- **`web_url` is always in the response.** Use it verbatim; do not reconstruct URLs from org domain + folder name.
- **Same-name uploads** trash any existing file with that name at the same path (recoverable). If two files with identical names must coexist, rename before uploading.
