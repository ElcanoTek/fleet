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

**Branching a teammate's chat copies only the transcript.** The fork carries
user/assistant text and no image references — the same filter the read applies
— and inherits the parent's lockdown. The owner's own branch still copies their
history in full. Without that split the filter would have been decorative: one
click turns a redacted read into a full-history chat the brancher owns and can
read without any filter at all.

What a teammate gets is a **read-only view** of the transcript — the same
renderer a public share link uses, reached through team membership instead of a
URL — with a banner naming the owner and the team, and one forward action where
the composer would be: **Branch to continue in your own chat**. The branch is
theirs from the first byte, filed into the same project, private until they
share it, and unaffected if the original is later unshared or deleted. The
owner's chat is never modified: conversations keep exactly one owner.

**What team sharing exposes is the transcript only.** Tool calls, tool results
and reasoning are filtered out server-side (the same filter the public snapshot
applies), and attachments and generated files stay behind the owner-scoped
workspace route. A shared conversation *about* a report does not hand out the
report.

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
`internal/store/team_sharing.go` for where.

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
| `POST /conversations/{id}/share-with-team` | owner | the opt-in; stamps the owner's team as the audience. `409` when there is no team or no team-shared home; the response reports the state it **stored** |
| `GET /conversations/{id}/team-view` | a teammate (or the owner) | the read-only transcript |
| `POST /conversations/{id}/branch` | anyone who can *read* the parent | fork into a chat you own |
| `GET /projects/{id}/team-conversations` | members | the project home's Team section |

Every refusal on the read paths is a `404`, indistinguishable from "no such
chat" — team membership is not probeable from here.

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
  `ListProjectsForUser`, which matches the owner regardless of team).
- **Deleting a team-shared project confirms too**: how many team learnings die
  with it, how many chats from how many members leave it and become temporary,
  and the existing export offered inline.
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
- **No file sharing through a team share.** Transcript only, deliberately.
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
