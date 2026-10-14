# File-edit safety (#787, #1745)

`edit_file` used to silently replace the **first** occurrence of `old_text`, had
no stale-content guard, and returned only a one-line "replaced N occurrence(s)".
A short `old_text` like `return nil` could therefore modify the wrong location
with no signal, and a concurrent change between an agent's read and edit was
overwritten silently. This hardens the tool (Stage 1 of the issue) on top of
the #784 sandbox FileOp seam — all three file tools already execute inside the
sandbox, so this is a behavior-contract change in the seam + tool layer, not a
new I/O path.

## What shipped

- **Unique-match requirement.** With `replace_all=false` (the default),
  `old_text` must match **exactly one** location. More than one → the edit is
  rejected with the match count and a request for more surrounding context (or
  `replace_all=true`). Zero → the existing "not found" error, now with a CRLF
  hint when the text would match after line-ending normalization.
- **No-op rejection.** `old_text == new_text`, or a replacement that leaves the
  file byte-identical, is rejected instead of writing.
- **Stale-content guard.** `edit_file` accepts an optional `expected_hash`
  (SHA-256, as returned by `view_file`/`edit_file`/`write_file`). If set and the
  file changed since, the edit fails without modifying it. It is **opt-in**:
  fleet's tools are stateless across calls (no enforced read-before-edit), so
  the model passes the hash it last saw to get the guard.
- **Content versions everywhere.** `view_file` appends a `sha256=…/size=…`
  metadata trailer (the full-file hash, streamed so any size works),
  `write_file` returns the written content's `sha256` (and, since #1745, the
  `old_sha256` of a file it replaced), and `edit_file` returns `old_sha256` +
  `new_sha256`.
- **Bounded unified diff.** A successful `edit_file` returns a unified diff
  (bounded to a few KB; large edits show a truncation marker and still apply in
  full) plus `+added/-removed` line counts.
- **Atomic, mode-preserving write** (already delivered by #784 and relied on
  here): same-directory temp + `os.replace`, preserving an existing file's mode
  (a `chmod +x`'d script keeps its execute bit through an edit).

## `write_file` overwrites (#1745)

`write_file` used to replace a file without a sign. The per-conversation
workspace persists across turns and holds the user's uploads and earlier
outputs, and after compaction the model may not remember that a name is taken,
so a write to it destroyed the file and the transcript showed only "Successfully
wrote N bytes". The same seam now reports an overwrite and can guard it:

- **An overwrite says so.** A write that replaces a file returns
  `Overwrote existing file <path> (old_sha256 …, N bytes) with M bytes
  (+a/-r lines)`, the new `sha256`, and a unified diff bounded like
  `edit_file`'s (4 KB, then a truncation marker; the write is applied in
  full). A new file reads `Created <path> (N bytes)`, and rewriting identical
  content says so. There is no diff for binary content (either side not valid
  UTF-8), or when either side is over 256 KB: the hashes and sizes are still
  reported, with a note saying why there is no diff.
- **`expected_hash` on `write_file`.** Optional, with `edit_file`'s meaning:
  the SHA-256 the file must have now (an optional `sha256:` prefix, any case).
  If the file differs, or is gone, the write fails (`ErrFileOpStale`) and
  nothing is written: no file, and no missing directory, is created. The hash
  is checked again just before the atomic rename, as for `edit_file`. The value
  `"absent"` makes the write create-only: it fails if anything exists at the
  path. `edit_file` refuses `"absent"`, since it edits a file that exists.
- **Overwriting stays allowed by default.** There is no `overwrite=true` flag
  and no read-before-write requirement; like `edit_file`'s, the guard is
  opt-in.
- **How the old file is read.** Through the parent directory descriptor the
  write uses, no-follow and non-blocking, and checked to be the regular file
  `lstat` saw, so a symlink or FIFO swapped in cannot redirect the read or hang
  the executor (a swap is refused as stale, a symlink as unsafe). It is hashed
  as it streams; its bytes are kept only up to 256 KB, for the diff. A
  non-regular entry already at the path (a FIFO, a socket) is replaced as
  before, reported without a hash, and never matches an `expected_hash`.
- **A file that appears meanwhile.** If an entry appears at the path between
  the executor finding none and the rename, the write is refused as stale,
  rather than replacing it and reporting a create.
- **Stale messages.** A stale refusal now carries only the executor's
  explanation (`FileOpStaleError`); `edit_file`'s used to repeat "file content
  has changed since it was last read" twice.
- `view_file`'s metadata trailer now points at `write_file` as well as
  `edit_file`.

Not done, deliberately:

- **No required guard.** Overwriting a user upload does not require
  `expected_hash` (an open question in the issue), and there is no
  read-before-write gate: the issue's source paper does not show that the
  extra friction helps.
- **Without `expected_hash`, the old file is not re-checked** between its hash
  and the rename (the same as `edit_file` without one), so the reported old
  hash and diff describe the file as it was read.
- **Every write through the seam pays the extra read**, the native tools that
  write files included (`download_url`, `generate_image`, `xlsx_workbook`,
  model-output artifacts), although only `write_file` shows the report: one
  streamed read of a file being replaced.

## Deliberately deferred

- **Stage 2 — `apply_patch` / all-or-nothing `multi_edit`.** A structured
  multi-file/multi-hunk primitive is a follow-up: it adds a tool to both
  rosters (the interactive chat runs near the 128-tool ceiling) and deserves
  its own validation/rollback matrix.
- **Stage 3 — formatter/LSP/compiler diagnostics after an edit.** Out of scope;
  the issue says it must not gate the correctness fixes.
- **Enforced read-before-edit.** Stateless tools + a ctx carrying only the
  conversation id make a cross-call read registry a larger change;
  `expected_hash` is the shipped opt-in guard.
- **Normalized CRLF/Unicode matching.** Only a diagnostic hint ships; matching
  stays byte-exact (which is what makes CRLF preservation trivially correct).

## Notes

- `ValidatePath` resolves symlinks, so an edit through a symlink lands on the
  **target** file (the symlink is unchanged). The atomic rename replaces the
  target's inode, so hard-linked copies diverge — expected.
- Changing the tool schemas (`expected_hash`, reworded descriptions; again for
  `write_file` in #1745) invalidates the provider prompt cache once on deploy,
  which the prompt-cache contract allows.
