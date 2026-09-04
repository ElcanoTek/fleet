# ADR-0058: Chat attachments are scoped per user and staged per conversation; the uploads tree is mounted nowhere

- **Status:** Accepted
- **Date:** 2026-09-04
- **Deciders:** fleet maintainers
- **Narrows:** [ADR-0036](0036-sandboxed-file-tools-and-host-io-exceptions.md) —
  the sandbox's read-only mount set loses the chat-attachment uploads root, and
  the one host-side read of a CLIENT-NAMED upload path (vision input, via
  `agent.loadImageAttachments` and its documented caller contract) gains an
  ownership gate. No exception class is added, and no host read moves.
- **Relates to:** [ADR-0049](0049-kubernetes-backend-split-control-plane.md)
  (which already staged attachments into the workspace claim on that backend —
  this generalizes it), [ADR-0057](0057-team-shared-chats-live-in-team-shared-projects.md)
  and [docs/TEAM-SHARING.md](../TEAM-SHARING.md) (branching a teammate's chat
  copies only the transcript), [docs/SHARED-FILES.md](../SHARED-FILES.md) (the
  staged-tree pattern), [docs/ATTACHMENT-SCOPING.md](../ATTACHMENT-SCOPING.md)
  (the design note)

## Context

A QA pass on team-shared chats reported an S1. The fixture: a chat whose
`run_python` output contained a marker string that appeared **nowhere** in the
user or assistant text, with the source CSV attached by the chat's owner. A
teammate branched that chat — a transcript-only copy, by ADR-0057 — and asked
what the marker was. The agent answered correctly.

The branch's transcript copy was clean: no tool calls, no tool results. But the
copied **first user message** still carried the owner's injected attachment
block, absolute path included:

```
---
**User attached files:**
- `fleet-team-share-test.csv` (…, /var/lib/fleet/data/attachments/uploads/fq6…/fleet-team-share-test.csv)
```

and the branch's `run_python` opened that path and read the rows.

Two separate defects met here.

**(a) Server-injected context lived inside the user's message text.** Every
chat turn appends server-derived blocks to the user's message before the model
sees it: the attachment manifest, the workspace inventory, the shared file
library announcement, expanded `@file`/`@url` context handles, the
skill-invocation note, connector recommendations. All of it was concatenated
into `messages.content.text`, which made it indistinguishable from what the
user typed — so the branch copy carried the owner's context, exports carried
it, and the transcript rendered an admin-published library listing inside the
user's own bubble (the same root cause as the separate finding about that
bubble).

**(b) The uploads tree was one flat, unscoped, sandbox-visible directory.**
Uploads landed at `<EmailAttachmentDir>/uploads/<random token>/<name>`, with no
user or conversation in the path. Two consequences:

- The whole `uploads/` root was bind-mounted read-only into **every** sandbox
  (podman), at the same absolute path, so any conversation's `bash`/`run_python`
  could read any user's upload given nothing but the path.
- `validateAttachments` confined a client-supplied path to that root and
  nothing more. So any authenticated caller who learned a path could name it in
  their **own** `/chat` request — pulling the file into their turn, and, for an
  image, straight into their model context host-side via
  `agent.loadImageAttachments`.

Paths travel. They rode inside message text into copies, exports and shared
transcripts. "Knowing the path" was the entire access control.

## Decision

**1. Chat attachments are staged into the sending conversation's workspace, and
the uploads tree is mounted into no sandbox.**

`stageAttachmentsIntoWorkspace` — which the kubernetes backend already used,
because a pod cannot see control-plane host paths at all — now runs on **both**
backends. Each validated non-image attachment is copied to
`<workspace root>/<conversation id>/attachments/<name>`, and the prompt block
advertises that path. `sandboxReadOnlyMounts` no longer includes the uploads
root, and *refuses* it (and any doc dir configured inside it) rather than merely
omitting it, so the boot-time mount list cannot quietly regain it.

An uploads path therefore resolves to nothing inside a sandbox: the fileop
anchor refuses it (`fileOpAnchorFor` finds no bind mount containing it) and
`bash`/`run_python` get ENOENT. Attachments remain reachable exactly from the
conversation they were attached to.

**2. The uploads tree is scoped per user, and containment is the ownership
check.** Uploads land at `<EmailAttachmentDir>/uploads/<sha256(email)[:32]>/<token>/<name>`,
and `validateAttachments` confines a claimed path to **the caller's own**
subtree. Naming another user's upload is a rejection, before staging and before
any host-side vision read. The segment is derived from the authenticated
identity, never from the request.

**3. Server-injected context is stored in its own column, not in the message
text.** Migration 056 adds `messages.injected_context` as a **nullable column
with no default**, and that nullability is load-bearing rather than incidental:
`NULL` is the legacy discriminator. Every write since the split supplies a
value — the derived suffix, or `''` for a turn that injected nothing — so
`NULL` means exactly "written before the split existed, and the blocks may
still be inside `content.text`". Do not "tidy" this into `NOT NULL DEFAULT ''`:
that erases the distinction, and the marker-based legacy strip below would then
have to run against every message ever written, including ones typed after the
migration, where a user who legitimately writes a separator followed by
`**Shared file library**` would lose that text and everything after it from a
branch copy. `content.text` keeps what the user typed; the suffix sits beside it on
`agent.HistoryEntry.InjectedContext`. `agent.ComposeUserMessage` is the single
place the two halves are joined for a provider call, and it reproduces the old
byte layout exactly, so replayed turns are byte-for-byte what they were. (The
cacheable prefix — system prompt plus tool definitions — is untouched; the
message tail the rolling recency breakpoints cache is unchanged.) The branch copy does not select the column, and legacy rows (blocks
still inside the text) are stripped by marker at branch time — and ONLY those
rows, selected as `injected_context IS NULL`. The strip cannot tell an injected
block from a user who typed one, so gating it on the discriminator is what
keeps a message written today byte-identical in its copy.

`HistoryEntry.InjectedContext` is `json:"-"` deliberately: the same struct is
projected into the public share snapshot and the team-shared read view, both of
which expose the transcript only. The owner's own conversation read opts in
through a dedicated response shape (`injected_context`, a string), which is
what lets a client render injected context outside the user's bubble.

**4. A copy that may not carry a message's content writes nothing.** Where the
branch filter removes everything a row held (an image-only user turn on the
cross-user path; a legacy attachment-only turn), no row is written — instead of
an empty bubble standing in for content the copy is not allowed to carry.

## Alternatives considered

- **Mount only the relevant uploads subtree per conversation.** A sandbox's
  mounts are fixed when its container starts, and the warm pool builds one
  `ContainerConfig` at boot and hands warm containers to whichever conversation
  claims one next (`internal/sandbox/pool.go`). Per-conversation mounts mean
  per-conversation containers: every turn pays a cold start, and the warm pool
  — the thing that keeps first-token latency tolerable — stops existing. On the
  kubernetes backend it would also mean a per-conversation subPath in a pod spec
  built per turn. Rejected: it buys the same containment the staged copy buys,
  at the cost of the pool.
- **A host-side broker read instead of a mount.** ADR-0036 moved file I/O *into*
  the sandbox precisely so model-selected reads inherit the container's runtime,
  seccomp, caps and limits. Answering a model's file read from the host would
  add a new host-side exception class for the most model-driven operation there
  is. Rejected: wrong direction.
- **Keep the mount, add a per-conversation allowlist on the host.** The mount is
  what `bash` and `run_python` see; a host-side check cannot mediate an
  in-container `open()`. It would have gated the file *tools* and left the two
  shells wide open. Rejected as security theater.
- **Hard-link instead of copy when staging.** Saves the bytes when data dir and
  workspace share a filesystem, but publishes the upload's inode under a
  read-write mount and makes "the staged copy" and "the original" the same
  file. Rejected (ADR-0055 flags the same hazard for shared files).

## Consequences

- **A copy per non-image attachment per conversation, on podman too.** Podman
  used to read the uploaded bytes in place. Now a send costs one copy (bounded
  by `FLEET_UPLOAD_MAX_BYTES`, default 1 GiB) and the bytes exist twice until
  the uploads TTL sweep reclaims the original. Both backends now behave the
  same way, which is also one fewer backend divergence.
- **Staged copies live as long as the conversation.** They are reclaimed with
  the conversation's workspace directory (`SweepOrphanWorkspaces`), not by the
  uploads TTL. The attachment prompt block says so. `SweepAttachments` and the
  admin storage report walk the same trees as before — the per-user level is
  just another directory to them.
- **A staging failure fails closed.** The entry keeps its uploads path, which
  now resolves nowhere inside a sandbox, so the agent reports a missing file
  instead of reading bytes through a tree no conversation owns.
- **Uploads written before the per-user segment validate for nobody.** They are
  ephemera between `POST /attachments` and the next `/chat` call; the blast
  radius is a composer message in flight across the upgrade, which drops the
  attachment exactly as any other unvalidatable entry and works on re-attach.
- **Legacy rows still contain paths in their text.** Migration 056 is additive;
  it does not rewrite history. Those paths no longer resolve in a sandbox
  (decision 1), and the branch strip removes them from copies, but an export of
  an old conversation still shows them.
- **Cross-conversation workspace visibility remains, unchanged by this ADR.**
  The workspace root is one read-write mount shared by every sandbox, with a
  directory per conversation inside it, so `bash`/`run_python` in conversation B
  can still read `<workspace>/<conversation A>/…` — including A's staged
  attachments — and can enumerate the conversation directories with `ls`. That
  is a pre-existing property of the workspace mount and applies to every
  agent-generated file, not to attachments specifically; the file *tools* are
  already confined to the turn's own root by ADR-0036's fileop anchor. Closing
  it means per-conversation mounts, i.e. the alternative rejected above, and is
  a separate change with its own performance decision to make. What this ADR
  fixes is that the paths in question are no longer handed to another
  conversation: they are not in copied messages, and they name a directory the
  brancher was never told about. See
  [docs/ATTACHMENT-SCOPING.md](../ATTACHMENT-SCOPING.md) for the honest scope.
