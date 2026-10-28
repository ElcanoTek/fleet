# Your own personas and the digital twin

**Status:** shipped 2026-10-28.

## Why

Personas used to be operator content only: YAML files in the client bundle's
`personas/` directory, picked from the composer. With always-on patrols
([CHAT-PATROLS.md](CHAT-PATROLS.md)) the assistant increasingly works while the
user is away, and the natural next ask is "run this the way *I* would" — a
persona the user writes, scoped to them, ideally drafted from how they already
work. This adds that without moving client content into the repo and without a
second governance path.

## What shipped

### Data: `user_personas` (migration `089_user_personas.sql`)

One row per persona, keyed `(user_email, name)`:

| Column | Meaning |
| --- | --- |
| `name` | lowercase kebab, ≤48 chars, fixed after creation — conversations refer to the persona as `user:<name>` |
| `display_name`, `description` | what the picker and the Personas page show |
| `instructions` | the persona's standing instructions (markdown, ≤32 KiB) |
| `base_persona` | the bundle persona it builds on (`''` = the deployment default) |
| `kind` | `twin` (thinks and writes as the user would) or `assistant` |
| `autonomy` | `advise` (default) or `act` |
| `patrols` | up to 8 suggested patrols `{title, cron, timezone, prompt}` — templates, never scheduled on their own |
| `status` | `active`, or `proposed` (drafted by the assistant, inert until approved) |
| `replaces_name` | on a proposal that edits an existing persona: the persona the draft will be written onto |

Limits: 50 personas per user. Suggested-patrol crons are validated with the
same rules a started chat patrol must meet (`validateAndComputeNextCron`: valid
zone, ≥5 minutes between runs), so a suggestion cannot be saved that would fail
when started. Deleting a user account deletes its personas.

### How a turn runs a user persona

`conversations.persona` keeps holding a string; a user persona is
`user:<name>`. Before each turn, `httpapi.resolveTurnPersona` resolves it
against the **conversation owner's** rows:

- `TurnInput.Persona` becomes the persona's **base** bundle persona, so the
  bundle YAML is still composed into the prompt and the manifest's per-persona
  tool policy (Gate-4, `agentcore/persona_filter.go`) still applies. A user
  persona adds instructions; it cannot reach a tool its base does not offer.
  This is the reason for the base design rather than a free-standing persona:
  letting a user persona skip the operator's narrowing would turn a prompt
  feature into a policy bypass.
- `TurnInput.UserPersona` carries the user's layer, which
  `agent.buildLayeredSystemPrompt` writes directly after the base persona
  section (`# Personal persona: <display name>`), with one framing line for the
  kind and one for the autonomy. A nil layer leaves the prompt byte-identical
  to before.
- A reference that no longer resolves — the persona was deleted, it is still a
  proposal, the conversation is a teammate's fork (the persona belongs to the
  original owner), or the base was removed from the bundle — runs on the
  deployment default persona instead of failing the turn. `agent.RunTurn` also
  refuses to read a `user:` reference as a file name, as a backstop.

New conversations (`POST /chat` first message, `POST /conversations`) refuse a
`user:` persona that is not one of the caller's **active** personas (400).
Bundle persona names are not newly validated — that behaviour is unchanged.

Patrols need nothing extra: a patrol turn is a turn on its conversation, so it
runs as that conversation's persona.

### Autonomy is guidance plus one default, not a permission

`advise` tells the model to analyze and recommend and to take no side-effecting
action unless asked in the conversation; `act` tells it to carry out routine
actions and report them. The only mechanical effect is the default for
**Start patrol** on the Personas page: `act` creates the patrol with the chat's
auto-approve on, `advise` with it off. Which tool calls stop for approval is
still decided by the conversation's approval setting and the deployment's
critical-tool policy, exactly as for any other chat.

### API (browser routes under `/api`, all scoped to the caller)

| Route | Purpose |
| --- | --- |
| `GET /user-personas` | the caller's personas (active + proposed), the workspace persona names and the default |
| `POST /user-personas` | create (active) |
| `PUT /user-personas/{id}` | replace an owned persona's fields (the name is ignored — it is fixed) |
| `DELETE /user-personas/{id}` | delete (also discards a proposal) |
| `POST /user-personas/{id}/approve` | turn a proposal into a persona; a draft that edits one is written onto it and the draft row removed, in one transaction |
| `GET /personas` | unchanged, plus `user_personas`: the caller's **active** personas for the picker |

`base_persona` is checked against the bundle's persona list on every write.

### `persona_studio` (interactive-only tool)

Registered in every interactive turn and excluded from scheduled runs (it is in
`interactiveOnlyToolNames`): its output is a draft for the person at the
keyboard. Actions:

- `list` — the caller's personas and the workspace personas to build on.
- `get` — one of the caller's personas in full.
- `history` — what the caller **typed** in their recent conversations (default
  25, at most 60 conversations; 12 messages each, 600 characters per message,
  40 000 characters in total). Assistant replies, tool output and
  server-injected context are excluded on purpose: they can carry text from the
  web or a connector that the user did not write, and a persona drafted from it
  would launder that text into standing instructions. Lockdown, archived and
  deleted conversations and patrol wakeups are skipped. The result tells the
  model the messages are data, not instructions.
- `propose` — stages a **proposed** persona (or, for an existing name, a
  `<name>-draft` edit). It returns a `/settings/personas?review=<id>` link. A
  proposal never runs until the owner approves it.

### Web

- **Settings → Personas** (`web/src/app/settings/personas/`): the digital-twin
  builder (a description, an opt-in to reading recent chats, and an autonomy
  choice; **Build with the assistant** opens `/chat?draft=…` with the request in
  the composer — nothing is sent until the user presses Send — and **Write it
  myself** opens the editor on the twin template), **Your personas** (editor
  with templates, drafts with **Approve**/**Discard**, **Start chat**, **Edit**,
  **View**, **Delete**, and **Start patrol** per suggested patrol), and
  **Workspace personas** (**Start chat**, **Customize**).
- **Start patrol** creates a conversation on the persona, `POST`s the patrol to
  `/api/conversations/{id}/schedules` (auto-approve per the persona's autonomy),
  runs the first check with `run-now`, and opens the chat.
- The composer's persona menu groups **Yours** above **Workspace**, shows
  display names, and links to the Personas page.
- `/chat?persona=<ref>&draft=<text>` opens a fresh chat on the persona with the
  draft in the composer and strips both parameters.

## Deliberately deferred

- **Operations Center tasks** still take bundle persona names only (the
  scheduled runner resolves persona files and the task form validates against
  the bundle). Recurring work on a user persona uses in-chat patrols.
- **Sharing personas with the workspace** (as user skills can be) — not built;
  a twin is personal by nature, and a shared persona needs a review story first.
- **Persona tool narrowing by the user** (a user persona with its own
  allow/deny list) — the base's policy applies; a user-defined narrowing could
  be added later without changing the safety argument, since it could only
  subtract.
- **An LLM-free twin draft** (summarizing history on the server) — the twin is
  drafted in a normal governed chat turn instead, so it runs under the user's
  model, cost ceilings and audit like everything else.
- **Changing a conversation's persona after its first turn** — unchanged: the
  persona is chosen when the chat starts.
