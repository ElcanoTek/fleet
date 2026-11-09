# Patrol Launchpad

The Patrol Launchpad is a guided wizard that takes a person from nothing set
up to an assistant on patrol over their tools: connected tools, saved
non-negotiables, a chosen watch list and rhythm, and a first check already
running. It opens from **Patrols** (sparkles) in the navigation rail (`NavRail`), or
from `?launchpad=1` on any page.

**The rail names the three ways of working**, each with a one-line subtitle,
because patrols and background tasks both run on a schedule and the names
alone would blur: **Chat** (*Talk it through*), **Patrols** (*Chats that check
in on a schedule*) and **Background tasks** (*Runs without a chat*).
"Background tasks" replaced "Operations Center" everywhere a user sees it — the
rail, the page header, the guide title and text, approval-card and tool text the
assistant relays, the owner-digest email, the README, the default bundle's
README and the installer's prompts (`scripts/bootstrap.sh`); the guides note
the old name once. URLs (`/orchestrator`, `/help/operations-center`), code
identifiers and comments, and the operator-only `fleet admin` CLI keep the old
name. The user guides moved
from the rail into the account menu, beside Settings.

**Patrols live in Chat; the rail item is their index.** A click reads
`GET /api/patrols` (the caller's active and paused recurring patrols across
their own non-deleted chats, each with its chat title; `store.ListUserPatrols`,
capped at 100, served by the partial index `idx_chat_schedules_user_patrols`
from migration 096, which `TestListUserPatrols_UsesItsIndex` pins). With none, it opens the wizard; with some, a menu lists them —
name, chat, cadence, *Paused* — each opening its chat, with **New patrol** at
the bottom. On a collapsed rail the menu opens beside the icon strip
(`right-start`, kept on-screen). The menu is capped to the viewport and
scrolls, so with many patrols the last rows and **New patrol** stay reachable.
Rows in the caller's own chats need no access check; a teammate's
multiplayer chat is checked once per request, however many patrols it holds.
Opening the wizard from that menu leaves the focus on the Patrols
item, so closing the wizard returns it there. On a phone that item is in
the drawer the wizard closed, so the focus goes to the visible **Open
sidebar** button instead. The wizard says where a patrol lives (in Chat, under its name, with
the Patrol bar on top) on its Plan and Launch steps and on the success screen. The wizard is portalled to `document.body` (the phone drawer is a
transformed element) and opens on the rail's dialog layer.

User-facing description: the Chat guide's "Patrol Launchpad" section
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`).

## What shipped

**Seven steps** (`web/src/app/launchpad/PatrolLaunchpad.tsx`):

| Step | What it does | API |
|---|---|---|
| Why | The preamble: an "expanding mind" ladder (chat → scheduled reports → patrols that analyze and decide → acting on your behalf). Each rung shows its corresponding panel from the Expanding Brain meme (`MindArt.tsx`, backed by `web/src/app/launchpad/assets/mind-1.webp` … `mind-4.webp`) alongside a punchy one-line quote of what you say to Fleet at that rung, followed by the trust path: start at Suggest & ask, turn on ⚡ Auto-approve in the Patrol bar later. The Watch step and the success screen repeat that nudge. | — |
| Plan | Lists the five steps ahead; shows how many tools are ready and memories saved. | `GET /api/mcp-servers`, `GET /api/memories` |
| Tools | Lists usable connectors (pre-ticked = on by default for a new chat; always-on rows shown locked), connectors that need setup with a **Set up** link, and **Connect another tool**. Both links open Settings → Connections in a new tab; the list refetches on window focus. | `GET /api/mcp-servers` |
| Ground rules | **Who's on watch**: workspace personas and the person's own (`GET /api/personas`). **Non-negotiables**: each is saved immediately as a memory of kind `constraint`, then pinned. | `POST /api/memories`, `PATCH /api/memories/{id}` |
| Watch list | Seven goal cards (`PATROL_GOALS` in `launchpadModel.ts`), free text, and the autonomy mode: report / suggest (default) / act. | — |
| Schedule | Six presets (weekday mornings by default) or the shared `PatrolSchedulePicker`; browser time zone by default. | — |
| Launch | Summary, editable name and instructions (a template draft from the answers), optional **Draft it with AI**, then **Launch patrol**. | see below |

**Launch** uses exactly the endpoints Settings → Personas' "Start patrol"
uses, in order: `POST /api/conversations` (title, model, persona if chosen) →
`POST /api/conversations/{id}/mcp-servers` (`enabled_optional` = the ticked
optional, usable connectors; always-on rows are never sent) →
`POST /api/conversations/{id}/schedules` (title, prompt, cron, time zone,
`auto_approve` = mode is "act") → `POST …/schedules/{id}/run-now`. A failed
run-now leaves the patrol armed. Every patrol rule (five-minute floor,
ownership, auto-approve semantics, retention exemption) stays the server's.

**Draft it with AI** — `POST /api/patrol-draft` (`internal/httpapi/patrol_draft.go`)
calls `agent.Manager.SuggestPatrolDraft` (`internal/agent/patrol_draft.go`): a
short-lived, structured-output, host-side model call with a 45-second timeout
on `RecurringTaskModel` (`FLEET_RECURRING_TASK_MODEL`; a patrol is a recurring
task run in a chat), metered as the `patrol_draft` aux-usage log line. Input is
the wizard's answers (goals, notes, the ticked connectors' names and
descriptions, cadence in words, mode, persona) plus the person's memories,
which the server reads itself and renders as a patrol turn receives them
(`memoryContents`: active only, pinned first, at most 50, each with its kind
and validity window). Each memory's content is shortened to 400 characters
before that annotation is added, so a long memory keeps it. If the memories
cannot be read the request fails (503) rather than drafting without them. The
body is capped at 64 KiB and each list is bounded. It returns
`{title, prompt, summary}` and persists nothing. A 502 leaves the template
draft in place and says so.

**Launch is careful about partial outcomes.** The dialog cannot be dismissed
while launch requests are in flight. If a step fails after the chat was
created, **Back to the plan** (or closing the dialog) cancels any schedule it
armed and deletes that chat, so a retry never leaves an orphan (or an armed
patrol) behind. Each undo step counts only when the server confirms it (or
says the thing is already gone). If one fails, the dialog stays open on an
error saying what is left, keeps it, and tries again on the next Back or
close; **Open the chat** stays available.
A create whose reply is lost (the request failed in transit, or the reply
could not be read) may still have made the chat, so every launch sends its
create with a fresh idempotency key (`client_request_id` on
`POST /api/conversations`). The key decides the new conversation's ID
(`store.ConversationIDForCreateKey`, a name-based UUID of the user and key),
so a retry lands on the same primary key. The server answers a known key
with the chat it made, and the chat and its key are bound by the one
INSERT, so no crash can separate them. A concurrent duplicate gets the first
one's chat. Where deletes are soft, a key whose chat has since been deleted
is refused (409); where they are hard (the default), the ID is free again
and the key would make a new chat. A new launch always uses a new key.

Known limits, both needing a lost request plus an unlucky overlap. A create
request that arrives *after* the wizard has settled and rolled back its chat
(on a hard-delete deployment) makes the chat again; nothing then rolls it
back, and it shows in Chat to delete by hand. A retry that lands while the
first create is still initializing the chat (its default connectors, say)
returns the chat early, so a later default write can overwrite the tools the
wizard then sets. Closing these needs a consumed-key tombstone and a
create-completion state; they were left out as out of proportion to the
risk. The wizard retries a lost create once at
once with the same key. If that also gets no answer, the dialog stays open
saying so, and Back or close sends it again to learn the chat's ID and roll
it back. Nothing is matched by title.

What a launch has created (a chat, a schedule, or a create whose outcome is
unknown) is also kept in session storage until it is settled. A reload at
the error screen therefore brings the wizard back to it ("Your last launch
didn't finish"), and Back or close cleans up as before, using the same key.

A non-negotiable is never saved twice: identical text (ignoring case) is
refused before any request, and a save whose reply was lost is looked for in
the memories before a retry is invited. If the
connector catalog never loaded, launch leaves the connector set the server
seeded at creation instead of replacing it with an empty one. A refused first
run (a full queue, say) is reported on the success screen — the patrol is
armed and runs at its next time — rather than claimed as running. A first-run
request that got no answer at all is reported as unconfirmed, and the copy
says to check the chat before pressing **Run now**, so a lost response never
invites a duplicate run. A
non-negotiable that saved but could not be pinned says so.

**Read-only viewers** (`GET /api/me` role `viewer`, newly routed through the
browser gateway) are moved back to the preamble, even with progress saved on
a later step, and see only it with a note that they cannot save
memories or start a patrol; every write the wizard makes is refused for that
role server-side anyway (`rejectViewerWrites`).

**Memories shown in the wizard** are the active, accepted ones only:
retired memories and unaccepted proposals (`source = "proposed"`) are listed by
`GET /api/memories` but never injected into a turn, so they are filtered out.

**Edits**: the patrol name and the instructions are tracked separately, so
renaming the patrol keeps the instructions following the plan. Custom (or
AI-drafted) instructions stay as written; if the plan changes afterwards the
review step says they may no longer match and offers **Rewrite from the plan**.

**State**: answers live in `sessionStorage` until launch, under a key scoped to
the signed-in account (`fleet.launchpad.v1:<email>`), so closing the dialog or
reloading keeps progress for the browser session and a different account
signing in on the same tab never sees or launches them.
`localStorage` remembers that the Launchpad was opened (`fleet.launchpad.seen`,
hides the pulsing dot) and that a patrol was launched
(`fleet.launchpad.launched`, changes the welcome kicker).

## Deliberately not done

- **No server-side onboarding state.** Whether someone "finished" onboarding is
  not recorded anywhere but their browser; the button stays available because
  the wizard is just as useful for a second patrol.
- **No automatic pop-up.** The wizard opens only on a click or `?launchpad=1`;
  first-time discovery is the pulsing dot.
- **Goal ↔ connector matching is a keyword hint** over the connector's name,
  label, description and category. It never gates a choice: a goal with no
  matching connector can still be picked, and both the template and the AI
  draft tell the assistant to report what it needs.
- **Connector setup is not embedded.** Credentials and OAuth stay on Settings →
  Connections, opened in a new tab.
- **Persona creation is not in the wizard** — it links to Settings → Personas.
- **The patrol's instructions cannot be edited in a form after launch**; as for
  any in-chat patrol, the person asks in the chat (`chat_patrol update`) or
  stops it from the Patrol bar.

## Tests

- `web/src/app/launchpad/launchpadModel.test.ts` — matching, selection,
  template draft, presets.
- `web/src/app/launchpad/PatrolLaunchpad.test.tsx` — the full walk, the launch
  call order and bodies, AI draft success and failure, progress persistence,
  a failed launch.
- `web/src/app/launchpad/LaunchpadButton.test.tsx` — the new dot and the deep link.
- `web/e2e/mocked/patrol-launchpad.spec.ts` — the walk through the real
  gateway and bundle, the deep link, and axe on every step in both schemes.
- `internal/httpapi/patrol_draft_test.go`, `internal/agent/patrol_draft_test.go`
  — the endpoint's refusals and body cap, the schema, output normalization and
  prompt rendering.
