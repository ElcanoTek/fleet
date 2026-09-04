# Attachment scoping, and where a turn's injected context lives

Two things a chat turn adds to what the user typed, and where each of them is
allowed to be readable from. The reasoning is [ADR-0058](adr/0058-per-conversation-attachment-scoping.md);
this page is the operator- and contributor-facing description of what actually
ships.

## The short version

```
POST /attachments (authenticated)
  → <DataDir>/attachments/uploads/<sha256(email)[:32]>/<token>/<name>   host-only
                                  └── the caller's own subtree, the ownership gate

POST /chat
  → validateAttachments(caller, …)     confines each claimed path to THAT caller's subtree
  → stageAttachmentsIntoWorkspace()    copies each non-image file to
        <WorkspaceRoot>/<conversation id>/attachments/<name>            what a sandbox reads
  → the "User attached files" block advertises the STAGED path
  → the block is stored in messages.injected_context, NOT in the message text
```

Nothing under `uploads/` is mounted into any sandbox, on either backend.

## Why attachments are staged instead of mounted

The uploads tree is one flat directory shared by every user and every
conversation. It used to be bind-mounted read-only into every sandbox at the
same absolute path, which made a *path* the only thing standing between one
chat's `run_python` and another user's file — and paths travel: into a copied
user message, an export, a branched transcript. That is exactly how the S1
behind ADR-0058 worked.

So attachments reach an agent the way shared files do
([SHARED-FILES.md](SHARED-FILES.md)): through a copy under the **workspace
root**, the one tree both sandbox backends make visible (the podman bind mount;
the kubernetes workspace claim). The copy lands in the *sending conversation's*
own directory, so an attachment is reachable from the conversation it was
attached to and the advertised path names that conversation.

The kubernetes backend already worked this way — a pod cannot see control-plane
host paths at all. This is that behavior generalized, which removes a backend
divergence as a side effect.

Costs, stated plainly:

- **One copy per non-image attachment**, bounded by `FLEET_UPLOAD_MAX_BYTES`
  (default 1 GiB). Podman used to read the bytes in place; now a large send
  costs a copy, and the bytes exist twice until the uploads TTL sweep reclaims
  the original.
- **Staged copies live as long as the conversation**, reclaimed with its
  workspace directory (`SweepOrphanWorkspaces`) rather than by the uploads TTL.
  The prompt block tells the agent exactly that.
- **A staging failure fails closed.** The entry keeps its uploads path, which
  resolves nowhere inside a sandbox, so the agent reports a missing file instead
  of reading through a tree no conversation owns.

Images are exempt from staging: their bytes reach the model host-side as vision
input (`agent.loadImageAttachments`) and never through a sandbox read. The
prompt block tells the agent not to `view_file` them.

## Why uploads are scoped per user

Reachability was only half the hole. `validateAttachments` confined a
client-supplied path to the uploads root and checked nothing else, so any
authenticated caller who learned a path could name it in their **own** `/chat`
request and have fleet stage it — or, for an image, read its bytes straight
into their model context.

Uploads now land under a per-user segment (`sha256` of the normalized email,
truncated), derived from the authenticated identity and never from the request,
and validation confines a claimed path to the caller's own subtree. Containment
*is* the ownership check, which is why there is no second lookup to forget.

The segment is a hash rather than the email because the tree shows up in
operator `du` output and in backups, and there is no reason for it to enumerate
who uses the box. It is a namespace separator, not a secret; nothing treats it
as one.

**Upgrade note.** Uploads written before this change sit at
`uploads/<token>/<name>` and carry no owner, so they validate for nobody. They
are ephemera whose lifetime is the gap between `POST /attachments` and the next
`/chat` call: the blast radius is a composer message in flight across the
restart, which drops the attachment (as it does any unvalidatable entry) and
works when re-attached. The TTL sweep reclaims the leftovers.

## Where a turn's injected context lives

Every chat turn appends server-derived blocks to the user's message before the
model sees it:

| block | source |
| --- | --- |
| `**User attached images**` / `**User attached files:**` | `httpapi.appendAttachmentsBlock` |
| `**Workspace files persisted from earlier turns**` | `httpapi.appendWorkspaceInventoryBlock` |
| `**Shared file library**` | `sharedfiles.PromptBlock` |
| `**Contents of @file:"…"**`, `**Fetched @url:…**`, `**Context handle notices:**` | `httpapi.appendContextHandleBlocks` |
| `[Skill invoked: …]` | `httpapi.matchSkillInvocation` |
| `**Possibly-relevant connectors (NOT currently connected):**` | `httpapi.appendConnectorRecommendationBlock` |

All of it used to be concatenated into `messages.content.text`. It now lives in
`messages.injected_context` (migration 056), beside the user's own words:

- **The model still sees both**, joined by `agent.ComposeUserMessage` in the
  governed prompt assembly (`assembleTurnMessages`) and recomposed the same way
  on replay (`replayHistory`). The join reproduces the old byte layout exactly,
  and a test pins that byte-parity. Nothing in the cacheable prefix moved — that
  is the system prompt plus the tool definitions
  ([PROMPT-CACHE-CONTRACT.md](PROMPT-CACHE-CONTRACT.md)), neither of which this
  touches — and the message tail the rolling recency breakpoints cache replays
  byte-for-byte as before, so the split costs no cache hits either.
- **The branch copy carries the user's text and none of the suffix.** The copy
  query does not select the column, on the owner's own fork as well as a
  teammate's: injected context is per-turn derived state naming another
  conversation's files, and the branch recomputes its own on its first turn.
- **Legacy rows are stripped by marker at branch time**
  (`agent.StripLegacyInjectedContext`), because rows written before 056 still
  embed the blocks in their text. The strip cuts at the earliest marker in the
  list above, and the blocks are always appended contiguously at the end, so it
  removes later blocks too — including ones added after that list was written.
  It matches the full separator + bold header, so a user who merely types
  `**User attached files:**` in a sentence is not truncated.
- **Nothing else publishes it by accident.** `agent.HistoryEntry.InjectedContext`
  is `json:"-"`: the same struct is projected straight into the public share
  snapshot and the team-shared read view, which expose the transcript only.

### API shape (for the transcript renderer)

`GET /conversations/{id}` returns each history entry as:

```json
{
  "id": 1234,
  "role": "user",
  "type": "text",
  "content": { "text": "what is the CPM by channel?" },
  "injected_context": "\n\n---\n**User attached files:**\n- `spend.csv` (…)\n"
}
```

- `content.text` — what the user typed, nothing appended.
- `injected_context` — `string`, **omitted when empty**, present only on user
  text entries. Render it *outside* the user's bubble (a collapsed system note)
  or not at all.

The live turn stream matches: the `user.message` SSE frame carries
`{"text": …, "injected_context": …}`, with the same omit-when-empty rule, so a
reload and a live turn agree.

Rows written before migration 056 have `injected_context` empty and the blocks
still inside `content.text`. A client must tolerate both and must not assume an
old conversation's user bubble is free of injected markup.

## What this does not fix

**Cross-conversation visibility of the workspace root.** The workspace root is
a single read-write mount shared by every sandbox, with one directory per
conversation inside it. `bash`/`run_python` in conversation B can therefore
still read `<WorkspaceRoot>/<conversation A>/…` — including A's staged
attachments — and can enumerate the conversation directories with `ls`.

Three things to keep straight about that:

- It is **pre-existing and general**: it applies to every file an agent
  generates, not to attachments. `EnsureWorkspaceDir` has always said the
  isolation is at the DB row layer, not the filesystem.
- The **file tools** are already confined to the turn's own root (ADR-0036's
  fileop anchor); it is the two shells that see the whole mount.
- What ADR-0058 changes is that those paths are no longer *handed* to another
  conversation. They are not in copied messages, exports, or shared
  transcripts, and they name a directory the other party was never told about.

Closing it properly means per-conversation mounts, which means per-conversation
containers — the warm pool builds one container config at boot and hands warm
containers to whichever conversation claims one next, so this is a real
performance decision, not a patch. It is a **separate change**, deliberately
not attempted here.
