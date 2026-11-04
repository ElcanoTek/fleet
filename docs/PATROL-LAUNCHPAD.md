# Patrol Launchpad

The Patrol Launchpad is a guided wizard that takes a person from nothing set
up to an assistant on patrol over their tools: connected tools, saved
non-negotiables, a chosen watch list and rhythm, and a first check already
running. It opens from the **Launchpad** button (sparkles) in the shared top bar
(`PageTopBar`) on Chat, the Operations Center, Settings and the Guides, or from
`?launchpad=1` on any of those pages.

User-facing description: the Chat guide's "Patrol Launchpad" section
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`).

## What shipped

**Six steps** (`web/src/app/launchpad/PatrolLaunchpad.tsx`):

| Step | What it does | API |
|---|---|---|
| Welcome | Explains a patrol; shows how many tools are ready and memories saved. | `GET /api/mcp-servers`, `GET /api/memories` |
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

**State**: answers live in `sessionStorage` (`fleet.launchpad.v1`) until launch,
so closing the dialog or reloading keeps progress for the browser session.
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
