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
  `old_sha256` of a file it replaced, when it read that file), and `edit_file` returns `old_sha256` +
  `new_sha256`.
- **Bounded unified diff.** A successful `edit_file` returns a unified diff
  (bounded to a few KB; large edits show a truncation marker and still apply in
  full) plus `+added/-removed` line counts.
- **Bounded diff time and memory.** The diff is taken before the edit is
  committed, from only the lines around the replacements, with three lines of
  context; the rest of the file is never decoded or diffed. Each replacement
  gets its own hunk unless its context lines touch another's, so a
  `replace_all` rename through a long file gets a hunk for every site, however
  far apart they are (the diff is still cut at 4 KB). When the lines around
  the replacements come to over 2,000 lines or 256 KB a side, the result has a
  note in place of the diff and the line counts ("no diff: the changes, with
  context, are over 2000 lines"), and the edit still applies in full; when
  they are not UTF-8, the note is "no diff: binary content".
- **Line numbers and the last line.** Hunk headers number lines as `grep -n`
  and `sed` do, counting only `\n`, for `write_file`'s diff too; a file whose
  lines end in a lone `\r` therefore diffs as one long line. A last line
  without a newline is marked `\ No newline at end of file`, as `diff` and
  `git` mark it; before, difflib ran it into the next line of the diff
  (`-last+LAST`).
- **Why, and what it costs now.** Before, the whole file was decoded and
  diffed after the commit. A `replace_all` changing every other line of a
  233 KB file took 62.8 s to diff, past the 60 s file-op timeout, so an edit
  that had landed was reported as failed and the sandbox was retired; and
  under the default 512 MiB sandbox memory limit, diffing a file of a few tens
  of MB ran out of memory after the edit had landed. Measured now: one change
  in a 69 MB file takes 0.09 s and 148 MiB at peak (the edit holds the old and
  the new file); a rename at six places across a 3,654-line file shows all six
  (+6/-6).
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
  content says so. The diff is computed before the commit. When the old file
  was not read (unreadable, not a regular file, or over 64 MB on a write
  without `expected_hash`, below), the report gives its size and a note
  instead of `old_sha256` and a diff.
- **When there is no diff.** Binary content (either side not valid UTF-8),
  either side over 256 KB, or a change spread over more than 2,000 lines a
  side: the hashes and sizes are still reported, with a note saying why.
  Only the lines that differ, with three lines of context, are diffed (the
  hunk headers keep the file's line numbers, counted at `\n`), so one changed
  line in a long file stays cheap. The line cap exists because difflib's cost
  grows with the square of the line count: a 256 KB file of short lines,
  every other one changed, took over 60 s to diff, past the 60 s file-op
  timeout.
- **`expected_hash` on `write_file`.** Optional, with `edit_file`'s meaning:
  the SHA-256 the file must have now (an optional `sha256:` prefix, any case).
  If the file differs, is gone, is not a regular file, or cannot be read, the
  write fails (`ErrFileOpStale`, or a write error for an unreadable file) and
  nothing is written: no file, and no missing directory, is created. The hash
  is checked again just before the atomic rename, as for `edit_file`.
- **`"absent"` makes the write create-only.** It fails if anything exists at
  the path, and a file created while the write is in progress is refused too.
  The last check runs just before the rename, so a file created in the
  instant between that check and the rename is still replaced. `edit_file`
  refuses `"absent"`, since it edits a file that exists.
- **Overwriting stays allowed by default.** There is no `overwrite=true` flag
  and no read-before-write requirement; like `edit_file`'s, the guard is
  opt-in. A write without `expected_hash` is never refused because the old
  file cannot be described: when it is unreadable (for example mode 0200), is
  swapped or deleted while it is read, or grows while it is hashed, the write
  goes ahead as before #1745 and says the old file could not be read. A
  non-regular entry at the path (a FIFO, a socket) is replaced as before,
  reported without a hash.
- **A plain write does not hash a file over 64 MB.** Without `expected_hash`
  the old hash is only a report, and the file op runs under a 60 s timeout
  whose expiry retires the sandbox: a multi-GB file on slow storage (a network
  volume on the kubernetes backend) could turn a write that used to be
  instant into a failed turn. Such a file is replaced and reported with its
  size and a note, no hash. A write with `expected_hash` still reads it in
  full, so a guard over a very large file on slow storage can still hit the
  timeout.
- **How the old file is read.** Through the parent directory descriptor the
  write uses, no-follow and non-blocking, and checked to be the regular file
  `lstat` saw, so a symlink or FIFO swapped in cannot redirect the read or
  stall the executor: a symlink is refused as an unsafe path, and any other
  swap is never described as the replaced file. It is hashed as it streams;
  its bytes are kept only up to 256 KB, for the diff.
- **No blocking on a FIFO, for any file tool.** `view_file` and `edit_file`
  open the file non-blocking and refuse anything but a regular file ("path is
  not a regular file"); a guarded write whose file turns into a FIFO before
  the rename is refused as stale. Before, a FIFO in the workspace made
  `view_file` wait until the 60 s file-op timeout, which retires the sandbox.
- **A file that appears meanwhile.** If an entry appears at the path between
  the executor finding none and the rename, the write is refused as stale
  ("a file appeared at this path …"), rather than replacing it and reporting a
  create.
- **Stale messages.** A stale refusal now carries only the executor's
  explanation (`FileOpStaleError`); `edit_file`'s used to repeat "file content
  has changed since it was last read" twice.
- **One wire encoder.** The podman, host and kubernetes backends now build the
  executor's request with the same function, so a field such as
  `expected_sha256` cannot reach one backend and not another.
- `view_file`'s metadata trailer now points at `write_file` as well as
  `edit_file`.

Not done, deliberately:

- **No required guard.** Overwriting a user upload does not require
  `expected_hash` (an open question in the issue), and there is no
  read-before-write gate: the issue's source paper does not show that the
  extra friction helps.
- **Without `expected_hash`, nothing is re-checked** between the old file's
  hash and the rename: a plain write replaces whatever is at the path then, as
  before #1745. The reported old hash and diff describe the file that was
  read, which a concurrent swap can make a different file from the one
  replaced.
- **Every write through the seam pays the extra work**, the native tools that
  write files included (`download_url`, `generate_image`, `xlsx_workbook`,
  model-output artifacts), although only `write_file` shows the report: one
  streamed read of a file being replaced (skipped over 64 MB, see above) and,
  for text up to the caps, a bounded diff.

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
