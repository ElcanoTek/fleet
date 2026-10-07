# ADR-0079: A team share carries the chat's outputs

- **Status:** Accepted
- **Date:** 2026-10-06
- **Deciders:** fleet maintainers
- **Supersedes:** ADR-0057 §4's "transcript only — attachments and generated
  files stay behind the owner-scoped workspace route", and its rejected
  alternative "Share the chat's workspace files with the team too (deferred)".
  Every other part of ADR-0057 stands.

## Context

ADR-0057 made a team share read-only and **transcript-only**: a teammate saw
user/assistant text, and every file link in it was redacted to a plain name
with "(file not shared)". It deferred file sharing with one sentence: "a
conversation about a confidential report must not become a download link for
it. A future pass could add per-file sharing with its own opt-in."

Users then did exactly what that left them: they shared a chat with the team,
and the teammate got a transcript that ended with "here is the report" and no
report. The workaround was a public link (which also carries no files) or an
off-platform drive — the very thing projects were meant to replace. The
Fleet Projects sharing spec (Oct 2026) settles it: **sharing a chat shares its
outputs, minus any the owner unchecks.**

Three questions the backend had to answer before that sentence could be true:

1. *What is an output?* Fleet cannot tell "a file the agent created" from "a
   file the agent wrote while working" — the workspace holds both, plus the
   bundle symlinks and the user's uploads. It CAN tell which files the agent
   **presented**: the ones it linked in a reply, which the UI renders as file
   chips.
2. *Where does per-file state live, and what is its default?* A shared chat is
   live; a file presented after sharing must go with it without the owner
   re-sharing.
3. *How is a file served to someone who does not own the workspace?* Every
   existing file route is owner-scoped. This is the first cross-user file read.

## Decision

**A team share carries the chat's outputs — the workspace files the agent
presented in its replies — except the ones the owner unchecked.**

1. **An output is a presented file.** The destination of a markdown link or
   image (inline or reference-style) in an assistant TEXT reply, resolved by a
   Go port of the web's `resolveScopedWorkspaceHref` (sandbox-prefix strip,
   absolute-URL bailout, `.`/`..` reject including encoded forms, per-segment
   decode), that exists right now as a regular file in the workspace, reached
   without traversing a symlink, and is **not under `attachments/`**. Uploads
   are never outputs: never shared, listed or counted. A file the agent wrote
   but never linked is not an output either: it stays a download-only entry in
   its owner's Sources. "Presented" means RENDERED as a link or image: replies
   are grouped and normalized as the chat renders them and parsed with a
   CommonMark + GFM parser (goldmark), so link-shaped text that renders as no
   link — a code span, a fenced or indented code block, an HTML comment or raw
   HTML, a backslash-escaped `[`, an image's alt text — is not a chip and never
   an output. Defining it by what the UI renders means "what the owner sees as
   a chip" and "what a teammate may download" cannot disagree.
2. **Per-file state is a set of exclusions; default is shared.**
   `conversation_output_exclusions (conversation_id, path)` (migration 070).
   An output presented after sharing is shared automatically, because the
   chat is live. An exclusion is independent of `team_visible`: it survives
   stop sharing, sharing again, archive and unarchive, and moves, so a later
   one-click share (the row pill, the getting-started card, the move toast)
   can never re-expose a file the owner held back. The share dialog's
   checklist decides exactly the files it listed in one write
   (`unshared_paths` + `listed_paths`; an exclusion for a file it did not list
   stands), and that write
   happens **before** the flag flips, so there is no instant in which a chat is
   shared with a file just unchecked.
3. **The download route re-checks everything, every time.**
   `GET /conversations/{id}/team-files/<path>` streams a file only when (a)
   the caller can read the chat through the team door right now — the exact
   `GetTeamVisibleConversation` gate (the owner's opt-in, the caller's team is
   the audience the owner named, not archived or deleted); (b) `<path>` is,
   by exact string match, a CURRENT output; (c) it is not excluded. The bytes
   are then read component by component with each directory proven to be the
   real directory that was `Lstat`ed and the leaf opened `O_NOFOLLOW` and
   proven to be the file that was `Lstat`ed — so the owner's sandbox, which can
   write this tree, cannot swap a shared name for a link to an upload or an
   unchecked output between check and read. `os.Root` alone would have
   followed a symlink that stays inside the workspace. Every refusal is a 404.
   Responses carry `X-Content-Type-Options: nosniff` and
   `Content-Security-Policy: sandbox`, and active document types
   (HTML/SVG/XML) are forced to download — on the Go side and again in the web
   proxy, because these bytes were written by someone else's agent.
4. **A teammate's branch copies the shared outputs.** Branching a teammate's
   chat copies every output shared at that moment into the new branch's
   workspace at the **same relative path**, so the links in the copied
   transcript resolve against the brancher's own workspace. The copies are the
   brancher's from the first byte; later unshares or deletions by the owner
   never reach them. The origin (source, owner, title as seen, time, copied
   and withheld files) is recorded in `conversation_branch_origins`, and on
   the branch's first turn the agent is told which files it has and that any
   other file the transcript mentions did not come with it. The owner's own
   branch is unchanged.
5. **Archive unshares.** `SetArchived(true)` clears `team_visible` and the
   stamped audience in the same statement. Every read gate already refused an
   archived chat, so nothing changes at the moment of archiving; what changes
   is unarchive, which now brings the chat back as Only you instead of
   silently re-exposing it — and every output presented in it since — to the
   team.
6. **Public links stay transcript-only.** Nothing in this ADR touches
   `GET /shared/{token}`; its snapshot has no files and no route serves one to
   it.

## Enforcement

- `internal/httpapi/outputs_test.go` — the parser against the TS rules
  (traversal plain/encoded/double-encoded, `%2F..%2F` inside a segment,
  `sandbox:` and `/opt/chat/workspace/<uuid>/` prefixes, `%20`, absolute
  URLs, anchors, NUL, backslash), the markdown shapes (inline, image, image
  inside a link, angle-bracketed, escaped, reference-style used vs unused,
  inline code, nested fences), assistant-text only, the on-disk filter
  (uploads, missing, directories, symlinks), and the no-follow opener.
- `internal/httpapi/team_files_http_test.go` — the team-files gate end to end
  (shared 200 with the headers; excluded, upload, unpresented, missing,
  traversal, other team, archived all 404; a shared name swapped for a symlink
  to an upload and a directory swapped for a link to `attachments/` both 404),
  the outputs endpoints, share-with-team counts, team-view files, the team
  link, the branch copy (same paths, withheld named and not copied, survives the
  source's deletion, the owner's branch copies nothing), Sources groups, and
  that the public snapshot carries no files.
- `internal/store/team_files_test.go` — exclusions survive stop/share again/
  archive/unarchive/move and die with the chat; archive unshares; a same-team
  move keeps sharing; branch origins, viewer branches, the first-turn note;
  the team-link statuses.

## Consequences

- **A teammate can now download what the owner's agent wrote.** That is the
  feature, and it is why the gate is three conditions re-checked per request
  rather than a capability handed out once, and why the share dialog shows the
  file count before the share and lets the owner uncheck any file.
- **Sharing is no longer "what you see is what you share" for the future.** A
  file the agent presents tomorrow in a shared chat is shared tomorrow. The
  owner is told so in the dialog ("Files it creates will be shared too"), and
  can uncheck it in Sources at any time.
- **The output set is computed, not stored.** Every listing re-reads the
  transcript and stats the workspace. A file the agent overwrites in place is
  the new bytes at the same name; a file deleted from the workspace stops being
  an output (and stops being downloadable) immediately.
- **A branch copy is bounded.** One branch copies at most 1 GiB; files past
  that, or a file that cannot be copied (for instance a path that would cross
  one of the workspace's bundle symlinks), are recorded as withheld rather than
  failing the branch.
- **`team-view` now names the project** (`project_id`, `project_name`) — the
  breadcrumb back to the project the viewer reached the chat through. The
  owner's persona, model and lockdown stay unserialized.
- **Unarchive no longer restores sharing.** A client that relied on archive
  being a pause of the share now finds the chat Only you; sharing again
  restores the owner's earlier file choices.

## Alternatives considered

- **Every file the agent created is an output.** Rejected: the workspace mixes
  scratch files, intermediate CSVs and plumbing with deliverables, and fleet
  has no record of which was meant for the user. Presented files are the ones
  the user saw offered as files.
- **Per-file opt-in (default unshared).** Rejected by the spec: a live shared
  chat whose new outputs each need a second action is a share that silently
  goes stale, and teammates were already finding transcripts that end in a
  report they cannot open.
- **Serve through the owner's workspace route with a team check bolted on.**
  Rejected: that route serves any regular file in the workspace, uploads
  included, and follows in-workspace symlinks. The cross-user route serves
  exactly the shared-output list and nothing else.
