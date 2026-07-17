# File-edit safety (#787)

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
  `write_file` returns the written content's `sha256`, and `edit_file` returns
  `old_sha256` + `new_sha256`.
- **Bounded unified diff.** A successful `edit_file` returns a unified diff
  (bounded to a few KB; large edits show a truncation marker and still apply in
  full) plus `+added/-removed` line counts.
- **Atomic, mode-preserving write** (already delivered by #784 and relied on
  here): same-directory temp + `os.replace`, preserving an existing file's mode
  (a `chmod +x`'d script keeps its execute bit through an edit).

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
- Changing the tool schemas (`expected_hash`, reworded descriptions) invalidates
  the provider prompt cache once on deploy, which the prompt-cache contract
  allows.
