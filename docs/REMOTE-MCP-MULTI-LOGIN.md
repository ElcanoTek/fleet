# Multiple logins for hosted (official) MCP connections

Issue #988. A user can now hold **several logins to the same hosted MCP
server** — a work and a personal GitHub, two Gamma workspaces — and choose
which one a chat or a scheduled task uses. The model is the one bundled
connectors already had (`<VAR>_<ACCOUNT>` env seats, `docs/CONNECTOR-PREFS.md`),
so operators and users learn one vocabulary: a **connection name** (`github`,
`gamma`) and, under it, **seats** labeled by **account** (`work`, `personal`).

## What shipped

**Data model** (migration `051_remote_mcp_seats.sql`)
- `remote_mcp_servers` gains `account` (public label, canonical
  `[a-z0-9_]{1,32}`; `''` is the unlabeled seat every pre-existing connection
  is) and `is_default`. Uniqueness moves from `(user_email, name)` to
  `(user_email, name, account)`; a partial unique index enforces exactly one
  default per `(user_email, name)`. Every existing row becomes its name's
  default, so nothing changes until a second seat is added.
- Each seat is its own row with its **own sealed credential** (OAuth tokens
  or API key) and its **own share grants**. Sharing "work" never exposes
  "personal". Tokens are never merged: a run mounts exactly one seat per
  name.
- `conversations.mcp_accounts` (migration `052`): a per-conversation
  `name → account` override map for the chat Tools picker. Works for bundled
  connectors too — this closes the "no per-conversation seat override yet"
  gap in `CONNECTOR-PREFS.md`.

**Runtime**
- A seat registers in a run under the bundle seat formula
  `agentcore.RegisteredMCPName(name, account)`: the unlabeled seat is
  `mcp_<name>_<tool>`, a labeled seat `mcp_<name>_<account>_<tool>` — the same
  shape a bundled named account already produces, so the agent loop, Gate-2,
  approvals and the broker treat both alike.
- `agent.RemoteMCPSelection` replaces the old `enabled map[string]bool`
  everywhere an overlay is opened: `Filter`/`Enabled` (interactive opt-in),
  `Accounts` (pins, `name → label`), `Exact` (labels are literal; used by
  approval re-execution). Resolution per name: pinned label → that seat;
  otherwise the `is_default` seat, else the unlabeled seat, else the first
  connected. **A pinned seat that is not connected is reported in `Skipped`
  (as its registered name) — never replaced by another account.**
- The broker protocol's `RemoteScopeSpec` carries `accounts` and `exact`
  (public labels; validated in `mcpbroker.validateScopeSpec`). The
  credential-owning child resolves seats itself; the parent only maps the
  returned registration names back to `{name, account}` for attribution.
- **Chat**: a conversation's override (or, for bundled connectors, the
  connections-page default) rides in `TurnInput.MCPAccountDefaults`; a remote
  name without an override mounts its default seat. Once the hosted overlay
  is up, `RunTurn` rebinds the approval stager with the composite broker,
  merged catalog and the mounted seats, so a staged card records the
  `{connection, account}` that actually ran.
- **Approvals**: execution of a card whose server is one of the user's hosted
  connections reopens a short-lived overlay pinned `Exact` to the recorded
  seat (`Manager.OpenApprovalRemoteMCPScope`). A seat that no longer resolves
  fails the approval with the seat named — the remote half of #167 residual 2.
- **Scheduled tasks**: `mcp_selection` may name a hosted connection with an
  account to pin that seat. Names in the selection that are not bundle
  servers are routed to the overlay as pins instead of to the bundle binder
  (which would reject them as unknown). Every other connected connection
  still mounts on its default seat — the pre-#988 "auto-available" behaviour
  is unchanged. A pinned name that is neither a bundle server nor one of the
  owner's connections fails the run loudly, like an unknown bundle server
  always has.

**API** (chat-server, proxied 1:1 under `/api/**`)
- `GET /remote-mcp-servers`: rows carry `account`, `is_default`.
- `POST /remote-mcp-servers` accepts `account`. A second unlabeled login under
  a name is refused with a message asking for a label (`409`).
- `POST /remote-mcp-servers/{id}/default` — make this seat the default.
- `PUT /remote-mcp-servers/{id}/account {"account": "work"}` — relabel.
- `GET /mcp-servers`, `GET /conversations/{id}/mcp-servers`: hosted entries
  are **one per connection name** with `accounts` (labeled seats),
  `default_account`, and (conversation route) `account`; bundled entries on
  the conversation route also gain `accounts` and `account`.
- `POST /conversations/{id}/mcp-servers {enabled_optional, accounts}` — both
  full replacements. An unknown seat is a **400**, not a silent drop.
- First `POST /chat` body accepts `mcp_accounts` next to `enabled_optional`.
- Orchestrator `GET …/mcp-servers`: hosted entries gain `accounts` and
  `default_account`.

**UI**
- Settings → Connections: own connections group by name, one row per seat
  with an account badge and a Default badge; "Set default", "Rename", and
  "Add another account" (label required; key too for api_key). Directory
  cards of added OAuth/api_key entries offer "Add another account"; the
  manual add form has an optional label. Shared rows show the label.
- Chat Tools picker: a seat select on rows that have labeled seats.
- Task modal: hosted rows keep "Connected" and gain the same Account select
  bundled rows have; picking a label writes `{server, account}`, "Default"
  removes the entry.

## Decisions worth knowing

- **AAD stays `(owner, url)`; the label is not part of it.** Labels are
  renamable and two seats of one user at one vendor are one trust domain —
  transplanting a ciphertext between them already requires DB write access,
  which also lets an attacker flip `is_default` or the grants. See
  [ADR-0050](adr/0050-remote-mcp-seats.md).
- **`""` in a selection means "the default", except under `Exact`.** The
  unlabeled seat is therefore reachable as a pin only through approval
  re-execution; a user who wants to pin it explicitly labels it first
  (Rename). Pickers list only labeled seats plus "Default (<label>)".
- **The default is owner-side.** A grantee of a shared seat sees the owner's
  `is_default` among the seats shared with them; a shared seat never becomes
  the default under a name the grantee owns seats for. There is no per-grantee
  default preference.
- **Registration names follow the seat mounted, not the request.** Setting a
  labeled seat as default changes the tool names a chat sees
  (`mcp_gamma_work_*`), exactly as a bundled `default_account` pref does.

## Honest scope

- No per-conversation seat pick for shared-with-me seats beyond what the
  owner shared; no per-grantee default.
- Hosted seats do not participate in the task credential allowlist (Gate-3)
  any more than hosted connections did before — the child's remote authorizer
  narrows tools only.
- The `"Add another account"` flow for tenant-URL and BYO-OAuth-client
  directory entries re-asks the placeholders / client id (no prefill from the
  existing seat).
- Not verified end-to-end against a live vendor in this change: the unit and
  DB-backed tests cover seat resolution, storage invariants, the picker
  routes, the broker spec, and the task pin routing; the OAuth round trip
  itself is unchanged.
