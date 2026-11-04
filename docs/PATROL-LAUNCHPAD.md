# Patrol Launchpad

The Patrol Launchpad is a guided wizard that takes a person from nothing set
up to an assistant on patrol over their tools: connected tools, saved
non-negotiables, a chosen watch list and rhythm, and a first check already
running. It opens from **Patrols** (sparkles) in the navigation rail (`NavRail`), or
from `?launchpad=1` on any page.

**The rail names the three ways of working**, each with a one-line subtitle,
because patrols and background tasks both run on a schedule and the names
alone would blur: **Chat** (*Talk it through*), **Patrols** (*Chats that check
in on a schedule*) and **Background tasks** (*Run headless, deliver by email*).
"Background tasks" replaced "Operations Center" everywhere a user sees it — the
rail, the page header, the guide title and text, approval-card and tool text the
assistant relays, and the owner-digest email; the guide notes the old name
once. URLs (`/orchestrator`, `/help/operations-center`), code identifiers and
the operator-only `fleet admin` CLI keep the old name. The user guides moved
from the rail into the account menu, beside Settings.

**Patrols live in Chat; the rail item is their index.** A click reads
`GET /api/patrols` (the caller's active and paused recurring patrols across
their own non-deleted chats, each with its chat title; `store.ListUserPatrols`,
capped at 100). With none, it opens the wizard; with some, a menu lists them —
name, chat, cadence, *Paused* — each opening its chat, with **New patrol** at
the bottom. The wizard says where a patrol lives (in Chat, under its name, with
the Patrol bar on top) on its Plan and Launch steps and on the success screen. The wizard is portalled to `document.body` (the phone drawer is a
transformed element) and opens on the rail's dialog layer.

User-facing description: the Chat guide's "Patrol Launchpad" section
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`).

## What shipped

**Seven steps** (`web/src/app/launchpad/PatrolLaunchpad.tsx`):

| Step | What it does | API |
|---|---|---|
| Why | The preamble: an "expanding mind" ladder (chat → scheduled reports → patrols that analyze and decide → acting on your behalf). Each rung has its own scene drawn from Flag icons (a chat bubble; a calendar and report; a pinging radar; a spinning galaxy around a filled brain), a one-line quote of what you would say to the assistant, and a brain badge that grows a size per rung. Then Dr. Ethan Mollick (linked to his *One Useful Thing* newsletter) and his term *capability overhang* (*Co-Existence*, 2026, linked to the publisher page), and the trust path: start at Suggest & ask, turn on ⚡ Auto-approve in the Patrol bar later. The Watch step and the success screen repeat that nudge. | — |
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
the wizard's answers only (goals, notes, the ticked connectors' names and
descriptions, the person's memories, cadence in words, mode, persona); the
body is capped at 64 KiB and each list is bounded. It returns
`{title, prompt, summary}` and persists nothing. A 502 leaves the template
draft in place and says so.

**Launch is careful about partial outcomes.** The dialog cannot be dismissed
while launch requests are in flight. If a step fails after the chat was
created, **Back to the plan** cancels any schedule it armed and deletes that
chat, so a retry never leaves an orphan (or an armed patrol) behind. If the
connector catalog never loaded, launch leaves the connector set the server
seeded at creation instead of replacing it with an empty one. A refused first
run (a full queue, say) is reported on the success screen — the patrol is
armed and runs at its next time — rather than claimed as running. A
non-negotiable that saved but could not be pinned says so.

**Read-only viewers** (`GET /api/me` role `viewer`, newly routed through the
browser gateway) see only the preamble with a note that they cannot save
memories or start a patrol; every write the wizard makes is refused for that
role server-side anyway (`rejectViewerWrites`).

**Memories shown and sent to the drafter** are the active, accepted ones only:
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
