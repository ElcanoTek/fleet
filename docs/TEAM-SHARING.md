# Sharing work inside a project — team-shared chats and team learnings

The design note for the Projects enhancement pass: what shipped, what it
deliberately does not do, and where each piece is enforced. The invariant it
introduces has its own record in
[ADR-0057](adr/0057-team-shared-chats-live-in-team-shared-projects.md); the
surfaces it builds on are [`PROJECTS.md`](PROJECTS.md) (ADR-0021),
[ADR-0013](adr/0013-team-rbac.md) (per-conversation team visibility) and
[ADR-0047](adr/0047-self-serve-team-membership.md) (who may set a team).

The pass front-loads **exposure over invention**: the sharing flag, the shared
memory store, and the retention exemptions all existed. What was missing was UI,
affordance, and copy — and, in three places, code that told the truth.

## The three things a project now answers

**Find past work.** Search over a project's chat lists; empty states that name
both filing paths (drag, or **Move to project** from a chat's ⋮ menu) and the
reason to bother — a chat in a project doesn't expire.

**Organize by campaign.** Unchanged from ADR-0021, plus the honesty fixes: the
bulk "Delete all unpinned" now skips project chats (it did not, which made
"project chats don't expire" false at the one moment it mattered), and removing
a chat from a project confirms that it becomes temporary again.

**Share learnings.** Two mechanisms, one vocabulary:

- **Share with team** — one chat, read-only, for people in your team.
- **Team learnings** — the project's shared memory, written by any member,
  visible to all, injected into every chat in the project.

## Vocabulary (use these words)

| Term | Means | Not |
| --- | --- | --- |
| **Share by link** | Anyone with the URL. Read-only. Badge: chain link. | "shared" |
| **Share with team** | Your team, read-only, inside a team-shared project. Badge: two people. | "shared" |
| **Team learnings** | The project's shared memory, as users see it. | "shared memory" (backend term) |
| **My memory** | The user's own personal memories. | "memories" |
| **Set team** / **Team** field | Self-serve create / admin assignment. | "join" |

A chat can be link-shared, team-shared, both, or neither. The two badges are
different shapes and each is always labeled with its audience — one unlabeled
chain link previously stood for the only scope that existed, and inside a
team-shared project it read as "my team can see this", which it never meant.

## Team-shared chats

A chat is shared with the team from **⋮ → Share…**, which opens one dialog
holding both scopes. The toggle is available **only for a chat inside a
team-shared project**; otherwise it is disabled with the sentence that fits the
case ("move it into a team-shared project" / "this project isn't shared with
your team — share the project first"). That narrowing is the product decision
recorded in ADR-0057: the project home's **Team** section is the one place a
teammate looks, so a team-shared chat with no project would be readable by
people with no surface listing it.

**Branching a teammate's chat copies the transcript and the SHARED files.**
The fork carries user/assistant text and no image references — the same filter
the read applies — and inherits the parent's lockdown. Since ADR-0079 it also
carries every output the owner has shared at that moment, copied into the
branch's own workspace at the same relative path so the links in the copied
transcript resolve (see "Files in a team share" below). The owner's own branch
still copies their history in full and copies no files — it never needed to.
Without that split the transcript filter would have been decorative: one click
turns a redacted read into a full-history chat the brancher owns and can read
without any filter at all.

What a teammate gets is a **read-only view** of the transcript — literally the
same renderer a public share link uses (`ReadOnlyTranscript`; the two differ
only in how they load the markdown pipeline, which is a parameter), reached
through team membership instead of a URL — with a banner naming the owner and the team, and one forward action where
the composer would be: **Branch to continue in your own chat**. The branch is
theirs from the first byte, filed into the same project, private until they
share it, and unaffected if the original is later unshared or deleted. The
owner's chat is never modified: conversations keep exactly one owner.

**What team sharing exposes is the transcript and the chat's outputs.** Tool
calls, tool results and reasoning are filtered out server-side (the same filter
the public snapshot applies). Files go with the share since ADR-0079 — but only
the files the agent *presented*, minus any the owner unchecked, and never an
upload; see "Files in a team share" below. This replaces the earlier
"transcript only" rule, under which a teammate got a chat ending in "here is
the report" and no report. A public link is unchanged: transcript only, never
files.

**The pairing is enforced by the store, not just offered by the UI.** Sharing
is refused (`409`, with the reason) unless the caller is in a team and the chat
is in a project shared with that team. Un-sharing is never refused. ADR-0057
records why this stopped being a UI-only narrowing: every rule that revokes a
share is keyed on the project, so a share with no project was swept by none of
them and displayed by nothing — permanent, and unrevokable from any screen.

**A team-shared chat always has a home.** Moving it out of its project, making
the project personal, deleting the project, leaving the team, or being moved to
another team by an admin all unshare it — in the same statement or transaction
as the change that caused it. See ADR-0057 for why, and
`internal/store/team_sharing.go` for where. A move between two projects shared
with the same team keeps the chat shared, files included
(`SetConversationProject` keeps the stamp when the destination's team matches
it — pinned by `TestMoveBetweenSameTeamProjectsKeepsSharing`).

**Archiving unshares too** (ADR-0079). `SetArchived(true)` clears the flag and
the stamped audience in the same statement. Every read already refused an
archived chat, so nothing changes at the moment of archiving; what changes is
unarchive, which brings the chat back as Only you rather than silently handing
the team everything presented in it since. Unarchive clears the flag and the
stamp too, whatever state the row is in, and an archived chat cannot be shared
at all: `share-with-team` with `visible: true` answers 409 ("an archived chat
can't be shared with your team; unarchive it first"). Stopping sharing is never
refused. The owner's file choices are kept, so sharing again restores them.

**And no chat is left filed in a project its owner cannot see.** That is the
other half of the same rule, and unsharing alone got it wrong. The rail lists
chats *through the projects the viewer can see*, so a chat whose `project_id`
points at a project the viewer has lost access to is rendered by **nothing** —
not Projects, not Temporary, not Archived. Unticking "Share with my team" on a
project therefore made every teammate's chats in it vanish from their own rail.
Nothing was deleted (re-ticking the owner's checkbox brought them straight
back), which is exactly what made it bad: a member lost access to conversations
*they own*, with no trace and no explanation, for as long as somebody else's
setting stayed off.

So every path that takes a member's access away now **unfiles** their chats —
`project_id = NULL`, back into their own Temporary list — in the same
transaction as the revocation, and **never deletes one to get there**:

| Path | What it unfiles |
| --- | --- |
| Untick "Share with my team" (`UpdateProject`) | every **other** member's chats; the owner's stay filed |
| Re-point the project at a different team (`UpdateProject`) | the old team's members' chats |
| Delete the project (`DeleteProject`) | everyone's — nobody can see a project that is gone |
| Leave the team (`SetOwnTeam("")`) | the leaver's chats in team-shared projects they don't own |
| An admin moves someone between teams (`SetUserRoleTeam`) | the same, on the path the owner doesn't control |
| Hand the project over (`TransferProjectOwnership`) | the outgoing owner's chats, when they are not in its team |
| Delete the account (`DeleteUser`) | every member's chats in the projects that go with it |

The two team paths share one choke point (`unshareOnTeamChangeTx`), and the two
project paths share one statement (`unfileChatsWithoutProjectAccessTx`), so a
third way to change a team or re-point a project cannot quietly miss it. "Can
see" is `ListProjectsForUser`'s rule restated: the project's owner always,
otherwise an exact match between the project's non-empty `team_id` and the chat
owner's `users.team_id`.

Two honest consequences. **An unfiled chat is temporary again** — it counts
against the unpinned cap and the TTL sweep will reap it unless its owner pins
or re-files it. That is the accepted cost, and it is precisely what deleting a
project has always done to members' chats; `updated_at` is bumped so the
retention clock starts when access is lost rather than leaving a months-old
chat instantly sweep-eligible. And **migration 055 backfills the rows the old
paths stranded**, unfiling every chat whose owner is neither its project's owner
nor a member of its project's team. It is idempotent and touches nothing whose
owner can still see its project.

**The untick confirm quotes real counts.** `GET /projects/{id}/impact` (the
same read the delete confirm uses) reports two extra numbers about the chats
somebody *other than the owner* has filed in the project:

| Field | Means |
| --- | --- |
| `chats_from_teammates` | how many chats making the project personal would unfile |
| `teammates_with_chats` | how many distinct people those chats belong to |

— the numbers the untick dialog's sentence is built from: *"{N} chats from
teammates will move to their unfiled chats."* They are a strict subset of the delete counts
(`chats` / `members` / `team_shared_chats` / `memories`) on purpose — one read,
one shape, nothing to keep in sync.

**A share names its audience; it does not infer one.** Opting in stamps the
owner's team onto the chat (`conversations.team_shared_with`, migration 054),
and every read compares that stamp against the *caller's* team. The flag alone
would have meant "visible to whatever team the owner is in right now", so an
admin moving the owner from one team to another would have handed the new team
everything the owner shared with the old one, silently. Two consequences worth
knowing: a user with no team cannot share (there is no audience to name — the
request is refused), and changing someone's team unshares their chats rather
than re-pointing them.

### Endpoints

| Route | Who | What |
| --- | --- | --- |
| `POST /conversations/{id}/share-with-team` | owner | the opt-in; stamps the owner's team as the audience. `409` when there is no team or no team-shared home. Body `{visible, unshared_paths?, listed_paths?}` — `unshared_paths` (with `visible: true`) is the checklist, applied to exactly `listed_paths` (default: the current outputs); exclusions for unlisted paths stand. Answers `{team_visible, shared_files, total_files}`: the state it **stored**, and the shared-file count (for an unshare, how many just stopped being shared) |
| `GET /conversations/{id}/outputs` | owner | the chat's outputs, newest first, each with its own `shared` state, plus `total` / `shared_count` |
| `POST /conversations/{id}/outputs/share` | owner | share or unshare one output: `{path, shared}` |
| `GET /conversations/{id}/team-view` | a teammate (or the owner) | the read-only transcript, plus `project_id` / `project_name`, `files` (every output; an unshared one is a name only) and `viewer_branch` |
| `GET /conversations/{id}/team-files/<path>` | a teammate (or the owner) | one shared output's bytes — see the gate below |
| `GET /conversations/{id}/team-link` | any signed-in user | where a team link lands them: `owner` / `open` / `not_on_team` / `not_shared` |
| `POST /conversations/{id}/branch` | anyone who can *read* the parent | fork into a chat you own; a teammate's branch also copies the shared files and answers with `branch_origin` |
| `GET /projects/{id}/team-conversations` | members | the project home's Team section; each row carries the caller's `viewer_branch` |
| `GET /projects/{id}/files` | members | Sources, grouped by chat (see below) |
| `GET`/`PUT /projects/{id}/my-state` | members | the caller's own card and Sources state for this project |
| `GET /projects/{id}/impact` | members | the counts both destructive confirms quote: what a delete destroys, and how many teammates' chats an untick would unfile |

Every refusal on the read paths is a `404`, indistinguishable from "no such
chat" — team membership is not probeable from here. The team link is the one
read that answers a non-reader with something other than 404, and it says
only what routing needs (below).

## Files in a team share

ADR-0079 records the decision; this is how it works.

**An output is a file the agent presented.** The destination of a markdown
link or image — inline or reference-style — in an assistant *text* reply, that
the web UI would turn into a workspace file chip, that exists right now as a
regular file in the chat's workspace, and that is not under `attachments/`.
"Presented" means **rendered as a link or image**, decided by a real parser,
not by link-shaped text. `internal/httpapi/outputs.go` groups the assistant's
text entries into the messages the chat renders (consecutive assistant text,
until a user message or a compaction summary — as `history.ts` does; a
teammate's view, a teammate's branch and a public link carry a content-free
`summary_boundary` entry in the summary's place, which splits the same way and
carries none of the summary), applies
the renderer's own pre-parse rewrites (`normalizeAssistantMarkdown` and the
unfenced-HTML-document wrap in `AssistantContent.tsx`), and parses each with
goldmark — CommonMark plus GFM and footnotes, the dialect react-markdown with
remark-gfm renders. Only `Link`, `Image` and URL-autolink nodes count, each
with the href the renderer would emit (escapes and entities resolved), and
references resolve to their **first** definition. So link-shaped text that
renders as no link is never an output: a backslash-escaped `\[x](a.csv)`, an
HTML comment or other raw HTML, a fenced or indented code block, a code span,
an image's alt text, an unused reference definition, an email autolink. The
href is then resolved by a Go port of `resolveScopedWorkspaceHref`
(`web/src/app/chat/ui/workspaceHref.ts`), so what the owner sees as a chip and
what a teammate may download cannot drift. The team view decides files at
**render** time, not by rewriting markdown source: the assistant renderer's
link and image overrides take the href react-markdown's CommonMark parser
produced and decide it by its workspace path (`decideReadOnlyFile`) — a shared
output becomes a team-files link or inline image, every other workspace
reference a locked name. Nested label brackets, an escaped `]` and balanced
parentheses in a destination are therefore parsed by the same grammar on both
sides. The public link enforces the same render-time rule (no workspace link
or image ever reaches its DOM) and also keeps its source redaction
(`redactUnsharedFiles`) as a belt-and-braces pre-pass — it may withhold more
than renders, never less.
Two consequences follow, both deliberate:

- **Uploads are never outputs** — never shared, listed in Sources, counted in
  a file line, checklist or toast, or downloadable by a teammate — even when a
  reply links one. The same holds for `user-skills/`, where fleet materializes
  the owner's PRIVATE skills (`user-skills/<name>/SKILL.md`) into every
  workspace of theirs: never an output, never copied into a branch (a branch
  names a linked one as withheld), skipped by the Sources walk, refused by the
  download gate, and rendered as a plain name in the team view, like an
  upload. These two (`attachments/`, `user-skills/`) are the only
  fleet-written real directories in a chat workspace; the bundle entries
  `EnsureWorkspaceDir` seeds (`protocols`, `personas`, `system_prompts`,
  `skills`, `shared`) are symlinks, which the no-symlink opener refuses and
  the Sources walk does not follow.
- **A file the agent wrote without presenting it is not an output.** It is
  never shared or counted, and stays a download-only row in its owner's Sources.

The output set is computed from the transcript and the disk on every read; it
is not stored. A file deleted from the workspace stops being an output at once.

**Discovery is bounded.** Each read considers at most the 500 most recent
distinct references (newest first — by message, and within a message by
position; uploads do not count toward the bound), because every one is opened and stat'ed and the read runs on the
outputs listing, Sources, the branch copy, the download gate and the team
view's 12-second poll. Past the bound, `GET /conversations/{id}/outputs`
answers `truncated: true`, `team-view` answers `files_truncated: true`, and
Sources reports its existing `truncated`. A file referenced only before the
bound is not an output for anyone: not listed, not copied into a branch (it is
withheld there), and refused by the download gate — the bound narrows what a
teammate can download, never widens it. The walk also stops READING after the
2,000 most recent rendered replies or 4 MiB of reply text, whichever comes
first (a reply the byte budget would cut is skipped whole, never parsed
without its start), and reports that through the same truncated flags — so a
long chat with no links costs a bounded parse per read, not a transcript-sized
one. The READ is bounded the same way: every route but `team-view` (which
renders the transcript anyway) loads only the rows discovery visits —
assistant text with its content, and user text and summaries as content-free
reply boundaries, newest first, paged, and stopped where the walk's own reply
and byte budgets stop it (`LoadDiscoveryHistory`) — never the whole history
with its tool results and reasoning. Discovery over that read is identical to
discovery over the full history.

**Escaped destinations.** CommonMark backslash escapes (`[r](my\_file.csv)`,
any ASCII punctuation, bare or `<…>`, inline or in a reference definition) are
removed before a destination is resolved — by the Go parser and by the web's
team-view rewrite alike — so the escaped and unescaped spellings name the same
output.

**Per-file state is an exclusion list, default shared.** Sharing a chat shares
all its outputs, including ones presented later — a shared chat is live, and its
files are too — minus any the owner unchecked
(`conversation_output_exclusions`, migration 070). An unchecked file stays
unchecked across stop sharing, sharing again, archive, unarchive and moves. The
"fast paths" (the row pill, the getting-started card, the move toast, a new
chat started shared) never touch exclusions; the share dialog's checklist sends
`unshared_paths` with `listed_paths` — the files it showed — and decides exactly
those (unchecked are excluded, checked are shared), while an exclusion for a
file the checklist did not show (missing on disk when the dialog loaded, past
the 500-reference bound) is left alone, so re-sharing with everything checked
cannot quietly re-expose it if it comes back. A client that omits
`listed_paths` is taken to have shown the chat's current outputs. Sources
toggles one file at a time.

**The download gate.** `GET /conversations/{id}/team-files/<path>` is the first
cross-user file read in fleet, and it re-checks three things on every request:
the caller can read the chat through the team door right now (the same gate as
`team-view`, checked without loading the transcript), `<path>` is — by exact
string match — a current output, and the owner has not excluded it. `HEAD` is
served through the same gate (headers only), on the Go side and in the web
proxy. The file is then opened refusing a symlink at *any*
component, each step proven to be the entry that was checked, so the owner's
sandbox (which can write the workspace) cannot swap a shared name for a link to
an upload or an unchecked file between the check and the read. The leaf is
opened non-blocking, so a name swapped for a FIFO in that window is refused at
once rather than hanging the read. Responses carry
`nosniff` and `Content-Security-Policy: sandbox`; HTML, SVG and XML are always
downloads, never rendered — on the Go side and again in the web proxy.

**The team view** lists every output in `files`, with `shared` saying whether
it is a live download or a locked name. A withheld file's size and date are
zeroed for a teammate: its name is already in the transcript, nothing else
about it was shared.

**The live poll is conditional.** The open view re-reads `team-view` every
12 seconds while visible. Every response carries a weak `ETag`, and the client
sends the last one back as `If-None-Match`; when nothing changed the server
answers `304 Not Modified` with no body and the view keeps what it shows. The
version behind the ETag comes from one light query under the **same** read gate
as the full snapshot, run *before* the transcript is loaded — a caller who may
not read the chat gets the same `404` whatever `If-None-Match` says, never a
`304`. It fingerprints the chat row (`updated_at`, title, owner, audience,
project and the project's name), the visible transcript (count and highest id
of the user/assistant text and summary rows), the exclusion set (count plus a
hash of the sorted paths), the caller's own latest branch of the chat (which
with the transcript decides `viewer_branch` / `changed_since`), and the
caller. A 200's ETag is the version read before its body was built, so a change
landing in between only makes the next poll refetch. **Not covered:** the
workspace on disk. An output's size and date, and whether a referenced file
exists yet, are read when the body is built, so a file that appears or changes
on disk with no new message and no exclusion change shows on the next poll
after a real change. In practice the agent writes a file and then presents it
in a reply — a new message. The web proxy forwards `If-None-Match` and passes
the `304` and its `ETag` through.

**Branch contents.** A teammate's branch gets every output shared at the moment
of branching, copied into its own workspace at the same relative path, as the
brancher's own files (0644 files, 0755 directories, written through an
`os.Root` on the new workspace; the source is read with the same no-symlink
opener as downloads; a file whose size or modification time changed while it
was being copied — truncated, or rewritten in place to the same length — is
withheld rather than copied as a mix of two versions). The team gate is
re-checked before each file: once the owner stops sharing (or archives) mid-copy,
every file not yet copied is withheld, and a file the owner unticks mid-copy is
withheld even though it was shared when the copy started (the exclusions are
re-read per file; a failed re-check withholds the rest). The discovery and copy run detached from
the request's cancellation, bounded at two minutes (files past it are
withheld), so a client that gives up mid-branch does not get a branch whose
files silently did not come. The copy moves 1 MiB at a time and checks that
bound between chunks; a read or write stuck on a stalled filesystem is
abandoned when the bound passes (both descriptors are closed, the partial file
is removed and withheld, and so is every file after it). If discovery itself
fails (the transcript or exclusions read errors or runs out of time), nothing
is copied and the origin is recorded with `withheld_truncated: true`, so every
reference in the branch renders locked rather than live. They never update: later unshares, edits or deletions by
the owner do not reach them. Unshared outputs are recorded as `withheld_files`
and stay locked names in the branch's transcript — as is every other workspace
reference the transcript links that the branch did not receive (an upload,
which is never copied or shared; a presented file missing on disk; one past the
copy budget), so none of them renders as a live link that 404s. Only the
messages the branch copied count — those up to and including the branch
point; a reply after it contributes neither copies nor withheld names. The
withheld list is bounded like discovery (the 500 most recent references); when
references were dropped the origin says `withheld_truncated: true`, and the
branch then renders a workspace reference live only if it is a copied file or
one of the branch's own current outputs — everything else is a locked name.
The origin — source, owner,
the title as the brancher saw it, time, copied and withheld files — is stored
in `conversation_branch_origins` and served as `branch_origin` on the branch
response and on `GET /conversations/{id}`, with `source_still_shared` so the
banner links back only while the original is still readable. On the branch's
**first turn** the agent is told which files it has and that any other file the
transcript mentions did not come with it (one-shot, via a latch on the origin
row); without that it reads a link to a withheld file and confidently tries to
open it. The note rides on that turn's user message, so a turn that fails
before its user message is committed hands the latch back and the next turn
carries the note instead.

**"You branched this."** `viewer_branch` (on `team-view` and on each
`team-conversations` row) is the caller's most recent branch of the chat that
still exists, with `changed_since` = the original gained *messages* after the
branch was made. Messages rather than `updated_at`, because a rename or a share
toggle also moves `updated_at`, and the banner says "has added messages since
you branched". Measured by message id against the source's highest message id
at branch time (`source_max_message_id`), not by timestamp, because both clocks
are whole seconds. Only rows the team view shows count (user and assistant
*text*): a branch is cut at the last visible text message, and a finished turn
writes its `turn_summary` (and tool rows) after that, which nobody reading the
team view could see change. There is no per-person read state.

**Sources, grouped.** `GET /projects/{id}/files` answers `groups`, one per chat
with files: the caller's own chats (`mine: true`, every non-upload file, each
flagged `output` / `shared` / `your_copy`, with `is_branch` and `branched_at`
for a teammate branch) and the teammates' chats shared with the caller's team
in this project (their shared outputs only — the same gates as
`team-conversations`, downloads through `team-files`). `file_count` and
`shared_count` count outputs only. The flat `files` list (the caller's own
files) is kept for older clients, and also skips uploads now. The UI decides
the order; the server returns both kinds. A chat's outputs are resolved
independently of the bounded workspace walk, so a walk that finds nothing
(its entry budget spent on a tree of empty directories, say) still lists every
current output; a chat is left out only when both are empty. Each half lists
at most the 50 most recently active chats with files, examining at most 200
chats to find them; past either bound the response says `truncated: true`
(and the additive `groups_truncated: true`). An optional `?focus=<chat id>`
(sent by "Manage in Sources") lists that chat's group even past both bounds,
but only when it is in one of the two listings above — the caller's own chat
in this project, or a teammate's chat passing the same team gates; any other
id (a teammate's private chat, say) is ignored like a chat with no files. If
the focused chat still is not listed, the panel simply shows the listing,
with nothing opened or highlighted.

**The team link.** `/chat?team=<id>` lands a signed-in teammate on the
read-only view. `GET /conversations/{id}/team-link` tells the client where to
go: `owner` (open it normally), `open`, `not_on_team` (the chat IS currently
shared; carries the audience `team_id` and nothing about the chat — no title,
no project), or `not_shared` (anything else: unknown id, private, archived,
deleted, unshared; carries the chat's project only when the caller can see that
project anyway). Signed-out visitors are routed through sign-in by the web
proxy; that half lives in `web/src/proxy.ts`.

**Per-person project state.** `project_user_state` stores, per user per
project, "Keep personal" (the getting-started card dismissed for good), whether
they have shared a chat in it (set server-side by a successful
`share-with-team`, never by the client), and which Sources groups they left
open — so it follows them across devices. Updates are merged under a row lock,
so two devices writing at once do not overwrite each other. The open-groups map
is capped at 500 chats; each write drops entries for chats that left the
project or were deleted, and if it is still full, stored choices the write did
not touch make room — a new choice is never refused. The rows go
with the user when the account is deleted.

## Team learnings

The project's shared memory has existed since ADR-0021 and, until this pass,
**had no viewing surface anywhere**: entries were written and injected, and no
screen listed them. There are now two, both the same list with the same
permissions:

- the **Team learnings** panel on the project home, beside Instructions so the
  two team-level layers read as a pair;
- a **Team learnings** tab in the composer's memories modal, so a member can see
  and manage them without leaving the conversation.

Every entry carries **who wrote it and when** (provenance was already recorded).
Actions are Pin · Edit · Retire · Delete. **Retire is the default remove**: the
entry stops being injected, the record survives.

**Permissions in one line: members manage their own entries; the owner manages
all.** Enforced in `internal/httpapi/projects.go` (`mayManageProjectMemory`),
not just hidden in the UI.

### Capturing one

Two paths, one model — both show the destination *before* saving, never a hidden
default:

- the **memory approval card** ("Save this memory?") gains a **Save to: My
  memory | Team learnings** control. Inside a team-shared project, Team
  learnings is preselected; the user can flip it;
- a **Save** action in the message action row (beside Copy · Regenerate ·
  Branch) opens the same picker, for capturing something the agent said without
  composing a "remember this" turn.

Existing personal memories can be promoted with **Move to team learnings**
(with a project picker when several apply). It **moves** rather than copies — two
rows saying the same thing would be injected twice in every project chat.

## The three context layers

A chat in a project is fed by three layers, in the order
`internal/agent/prompt.go` (`buildSystemPrompt`) assembles them:

1. **Instructions** — one field, owner-only, injected first.
2. **Team learnings** — the project's shared memory, tagged `[project]`.
3. **My memory** — the reader's own personal memories.

Layers 2 and 3 arrive together in the same "User Memories" block. The helper
copy under Instructions says exactly that. (It used to say "injected before
personal memories", which named two of the three and omitted the only
team-writable one.)

## Team management, corrected

The Projects modal used to offer to **create a team inline**, with copy telling
teammates to "join the same name" — a path the server refuses (ADR-0047: joining
is admin-granted; an existing name returns `409`). Team creation is a
once-per-account act, and two surfaces already own it. So:

- **The Projects modal is display-only about teams.** A teamless caller sees
  where to fix it, branched on their role — an admin can do it themselves
  (most fleet users are admins; sending them to ask someone else would be worse
  than useless), a member is told to ask.
- **Settings → Team tells the truth.** "Name a team to create it. Teammates are
  added by an admin in Settings → Admin → Users." The `409` is handled inline as
  "That name is already in use. An admin can add you to the team…" — never "that
  team exists", because a team-shared *project* can hold the name with no
  members left, and the server cannot say which.
- **Leaving confirms first**, quoting real counts: the team-shared projects you
  stop seeing, the chats of yours that stop being shared, and the fact that
  projects you own stay yours and stay shared with the team (verified against
  `ListProjectsForUser`, which matches the owner regardless of team). Your own
  chats filed in the projects you stop seeing are **unfiled** rather than
  hidden (see above) — they are still yours, in your unfiled chats. The confirm
  does not put a number on that one; `LeaveTeamImpact` reports the two counts
  above and nothing else, and this doc says so rather than implying a third.
- **Deleting a team-shared project confirms too**: how many team learnings die
  with it, how many chats from how many members leave it and become temporary,
  and the existing export offered inline.
- **Unticking "Share with my team" confirms with the same numbers**, narrowed
  to what it actually costs: the teammates' chats it unfiles
  (`chats_from_teammates` / `teammates_with_chats`), which move to their own
  unfiled chats rather than disappearing.
- **Admin → Users assigns a team from a list**, with an explicit "New team…"
  option. The field was free text, so "Testing" and "testing" silently became
  two trust groups — a difference that only surfaces later, as a project a
  teammate cannot see.

## Honest scope — what this pass does NOT do

- **No project-level "share new chats by default".** One mechanism answers "is
  my chat visible?" — per-chat opt-in. Revisit if members ask.
- **No write access for teammates.** Branch is the way to build on someone's
  chat. Co-authoring a live conversation is a different feature (one workspace,
  one sandbox, one cost ledger).
- **No file versions.** `report_v1.xlsx` and `report_v2.xlsx` are two files;
  overwriting a file in place changes what the shared name serves.
- **No "every file the agent created" sharing.** Only presented files are
  outputs (ADR-0079); a file the agent never linked stays private to its owner.
- **A branch copies at most 1 GiB.** Shared outputs past that budget — or any
  that cannot be copied, e.g. a path that would cross one of the workspace's
  bundle symlinks — are recorded as withheld, not copied, and the branch still
  succeeds.
- **No repeated-corrections detector.** See "The auto-proposal question"
  below — it is decided, not deferred.
- **No campaign lifecycle** (archive/complete), **no per-project bindings** for
  scheduled tasks / skills / eval sets, **no roles inside a project**, **no
  invitations**, **nothing cross-instance**. (Ownership transfer *was* on this
  list; it is built — see above.)
- **Portfolio-wide actions** ("apply this block list to every index deal") are
  the Datasets feature's shape, not this one — see [`DATASETS.md`](DATASETS.md).

## The auto-proposal question (P2), decided

The brief's P2 was: *"when members keep correcting the same thing, fleet drafts
a team learning, a member approves, and it lands in the same Team learnings
store"* — the mirror image of Item D, gated on Q2
(`FLEET_SELF_IMPROVE_ENABLED`). **We are not building the correction
detector.** Not for size — because most of the outcome already ships, and the
missing piece conflicts with the invariant this same pass establishes.

**The outcome is largely already delivered, by D1 + D2.** The chat agent
already has a `propose_memory` tool that stages a proposal a human approves —
that is the "Save this memory?" card. Before this pass it could only propose to
*personal* memory. It now offers **Team learnings** as the destination, and
preselects it inside a team-shared project. So "fleet proposes a team learning,
a member approves, it lands in the Team learnings store" is a thing that
happens today, without anyone remembering to save it. What P2 adds on top is
specifically the *trigger*: distilling from repeated corrections rather than
from the model noticing a durable fact.

**That trigger needs two things fleet does not have, and one it should not do.**

1. *There is no feedback primitive in chat at all.* The #516 loop is built on
   `task_feedback` — thumbs-down plus a critique box on a scheduled task's
   run. Chat has no thumbs, no rating, no critique: a search for one across
   `internal/httpapi`, `internal/store` and the chat UI returns nothing. P2
   would have to design and ship per-message feedback (table, migration,
   endpoints, UI, permissions) first. That is a feature in its own right,
   bigger than any single item in this pass.
2. *It lives in the other database.* `task_feedback` and
   `learned_instructions` are in the **sched** Postgres database, with its own
   migration system — deliberately separate from the chat store (ADR-0005).
   Only the distiller itself (`agent.Manager.DistillLearnedInstruction`, a pure
   `(prompt, critiques, prior) → instruction` call) ports cleanly.
3. *And the evidence it wants is other people's private chats.* "Members keep
   correcting the same thing" means reading across the members' chats in a
   project. Those chats are private — that is the rule ADR-0057 is built on,
   and the reason team sharing is a per-chat opt-in. Distilling a shared
   learning out of them would quietly undo it. Restricting the evidence pool to
   *team-shared* chats keeps the invariant but leaves too little signal to cross
   any sensible threshold.

So P2 is not a follow-up we are deferring; it is a design question we are
answering **no** to in its stated form. If the need resurfaces, the honest
version is "let a member turn a correction into a proposed team learning
explicitly" — one button on a message, no cross-user inference — and D1's Save
action is already most of that.

**Q2 is answered with it.** `FLEET_SELF_IMPROVE_ENABLED` gates the *task*-scoped
loop, which is an existing, unrelated feature. Nothing in Projects reads it,
before or after this pass. It stays **off by default**, for the reason it always
was: it feeds user-authored critiques through an LLM to draft an instruction, and
the staged-approval design is what makes that safe rather than the flag. Turning
it on for a given box is that box's operator decision about the task feature, not
a prerequisite for anything here. There is no pending decision blocking Projects.

## Ownership transfer

A project is owner-only to edit and delete. Until now it could not change
hands, which made "the owner left" terminal in two ways — the second worse than
the first:

- the definition **froze**: every mutation is owner-scoped, so nobody could
  rename it, change its instructions, re-share it, or delete it;
- deleting the departing account **destroyed the project outright**.
  `DeleteUser` detached every member's chats and deleted the project's shared
  memories along with the row — so the routine admin action for "X left the
  company" silently took the team's project and every team learning in it.

Both are fixed:

- `POST /projects/{id}/transfer {"to_email": …}` hands the project over. It
  changes **only** who may edit and delete — the team, the team learnings, the
  chats and everyone's access are untouched, because none of those are keyed on
  the owner. Two callers are authorized: the **owner**, and an **admin** —
  the admin path is the point, since a departed owner cannot click anything, and
  it is why the route sits *before* the membership gate (an admin is usually not
  a member). Anyone else gets the same 404 a non-member gets for any project
  subresource. For a team-shared project the target must be in that team, so a
  project can never end up shared with a team its owner is not in.
- `DeleteUser` now **fails closed** when the account still owns team-shared
  projects, and the admin Users tab surfaces it as a `409` naming them:
  *"transfer them to another member first, then delete the account"*. Personal
  projects still go with the account — nobody else can see them, so there is
  nothing to hand over and no one to lose.

The UI is a collapsed **Transfer ownership…** control in the project settings
dialog (it is a once-in-a-project action, not a routine one), backed by
`GET /projects/{id}/members` for the picker.

**The refusal names projects as data, not only as prose, because a name alone
was a dead end.** The `409` body is JSON:

```json
{"error": "this account still owns team-shared projects (Quant) — transfer them to another member first, then delete the account",
 "owns_shared_projects": [{"id": "…", "name": "Quant"}]}
```

The ids are the point, and **the transfer happens in that panel**, not through
a link. A link could not work for the caller this is written for: an admin is
usually neither the project's owner nor a member of its team, so every
membership-gated surface — the chat rail's project list, the project home —
answers them with a 404, and "Transfer alpha" would land on a page that cannot
show the project, let alone its transfer control. The two routes that *do*
authorize an admin are called from the refusal directly: `GET
/projects/{id}/members` for the picker and `POST /projects/{id}/transfer` for
the handover, after which the delete is retried automatically — unblocking it
is the only reason the control is there. The current owner is not offered as
their own successor, since handing the project back is a no-op that leaves the
delete blocked.

`GET /projects/{id}/members` had to move to make that work. It sat *behind* the
membership gate while carrying its own owner-or-admin check, so the check was
unreachable for exactly the caller it names: an admin got the same 404 a
stranger does. It is now dispatched before the gate, like `transfer`, and
authorizes itself the same way. That left the transfer half-reachable
otherwise — an admin could POST the handover but could not ask who to hand it
to, which is the entire content of the decision.

The `error` field keeps the exact sentence, so a client that predates the
structured body still shows the explanation; without ids it names the manual
step rather than offering a control that cannot work.

## Deviations from the brief

- **The Projects modal already had a shared-memory list** (per project row), so
  "no viewing surface anywhere" was not quite right. It is relabelled *Team
  learnings* like everywhere else; the real gap — a first-class panel with
  authors, dates, and pin/edit/retire — is the project home's.
- **"Delete all unpinned" did NOT skip project chats.** The brief listed this as
  a check; it was a bug. `DeleteAllUnpinned` and `DeleteAllMatching` now filter
  `project_id IS NULL`, matching what the TTL sweep and cap eviction already
  did, and what the rail's Temporary list shows.
- **`Branch` was owner-only**, as the brief suspected. It now accepts any parent
  the caller can read.
- **The Team section needed a server-side project filter.** `ListTeamConversations`
  returns every team-visible chat across all projects; `GET /projects/{id}/team-conversations`
  is the scoped read, and it excludes the caller's own chats (they already render
  under "your chats", with a team badge).
- **Injection order, read from the code rather than the old copy:** Instructions
  first, then personal memories and `[project]`-tagged team learnings together
  in one block. The helper text says "then", not "before personal memories".
