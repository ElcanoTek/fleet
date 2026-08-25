# Unified connector enablement — availability, selection, binding

One mental model for turning connectors on and off, across every surface.
Three layers, each with a different lifetime:

| Layer | Surface | Lifetime | Question it answers |
| --- | --- | --- | --- |
| **Availability** | Settings → Connections | durable, per-user | "does this connector exist in my universe, and which credential account is my default?" |
| **Selection** | chat Tools picker | per conversation | "which of my available connectors are live in *this* chat?" |
| **Binding** | Operations Center task modal | pinned per task | "which connectors + account seats does this automation use, forever?" |

Chat is **supervised** — a human watches every turn — so it follows the user's
availability defaults and allows quick per-conversation narrowing. Scheduled
tasks are **unsupervised, one-shot automation** — they deliberately do NOT
follow anyone's preferences: each task pins an explicit `mcp_selection`
(`{server, account}` pairs) plus the Gate-3 `credential_allowlist`, and a
later preference change never rewrites a pinned task. The two use cases having
different enablement/permission models is intentional design, not an
inconsistency.

## Availability (`user_connector_prefs`, migration 032)

A row is an **explicit** per-user choice; absence means the operator default,
so the feature changes nothing until a user touches a toggle:

- `bundled` connectors (sandboxed, operator-shipped, keyed by server name):
  - **no row** → available in pickers; new chats default the connector on/off
    per the manifest's `enabled_by_default`.
  - **enabled=true** → available AND on by default in new chats; may carry a
    `default_account` — the credential-account seat the user's chat turns use
    (pre-existing conversations opted into the server pick the seat up on
    their next turn).
  - **enabled=false** → hidden from that user's pickers and dropped from
    their turns (an already-opted-in conversation still shows it so it can be
    turned off).
- `remote` connections (per-user hosted servers, keyed by server id — own or
  shared-with-me):
  - **no row / enabled=true** → wired into the user's runs as before.
  - **enabled=false** → excluded from that user's overlay. For a shared
    connection this is strictly personal — labeled "Off for me" — and never
    affects the owner or other grantees; the owner revoking the share (or
    deleting the server) remains the propagating action.

Prefs are a **preference, not an authority boundary**: the credential
allowlist and sharing grants stay the security gates. A stale seat (its env
vars removed) degrades to the default seat with no failed turn; the PUT
endpoint refuses an unknown seat up front.

## Surfaces

- **Connections page**: bundled connectors get "Enabled for me" / "Disabled
  for you" plus an Account seat picker (when the operator provisioned
  `<VAR>_<ACCOUNT>` seats) and a "Reset to default" that deletes the explicit
  row. Own remote connections get On/Off; shared-with-me connections get
  "On/Off for me".
- **Chat**: `GET /mcp-servers` (pre-chat) and
  `GET /conversations/{id}/mcp-servers` honor availability; the turn path
  drops disabled connectors and injects the user's default seat
  (`TurnInput.MCPAccountDefaults` → `MCPChoice.Account`).
- **Operations Center**: unchanged by design — the task modal's
  `McpServerPicker` (mode="task") continues to pin explicit
  `{server, account}` selections.

## API

- `GET /connector-prefs` → `{"prefs": [{kind, connector_id, enabled,
  default_account?}, …]}` (explicit choices only).
- `PUT /connector-prefs` upserts one; a bundled `default_account` is validated
  against the live catalog seats.
- `DELETE /connector-prefs?kind=&id=` reverts to the operator default.

## Honest scope

- Always-on (non-optional) bundled connectors render as visible-but-locked
  rows ("Always on") — nothing is invisibly enabled, and nothing about them is
  toggleable by design.
- The task modal's pickable set is not filtered by the task author's prefs —
  orchestrator identity is not guaranteed to map to a chat user, and tasks are
  an operator surface; revisit if the identities unify.
- The chat Tools picker can override the seat per conversation (#988,
  `conversations.mcp_accounts`, `POST /conversations/{id}/mcp-servers
  {accounts}`); without an override the seat comes from the availability
  default (tasks pin their own). Hosted connections use the same picker: their
  default is the seat flagged default on the connections page — see
  `docs/REMOTE-MCP-MULTI-LOGIN.md`.
