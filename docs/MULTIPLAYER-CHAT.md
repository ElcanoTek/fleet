# Multiplayer chat mode

Multiplayer chat mode lets multiple people on a team jump into the same live
conversation and send turns directly using their own signed-in identity,
personal memories, skills, and per-user MCP connector credentials — distinct
from read-only team sharing (`team_visible` + branch-to-continue) and public
share links (`/shared/{token}`).

## How it works

### Database & audience model (migration 092)

`092_multiplayer_conversations.sql` adds three columns to `conversations`:

- `multiplayer BOOLEAN NOT NULL DEFAULT FALSE`
- `multiplayer_participants TEXT NOT NULL DEFAULT '[]'` (JSON array of normalized emails explicitly added)
- `multiplayer_excluded TEXT NOT NULL DEFAULT '[]'` (JSON array of normalized emails explicitly removed)

When an owner enables Multiplayer mode on a conversation (`SetConversationMultiplayer`):

1. If the owner belongs to a team (`users.team_id != ''`), the conversation is
   stamped with `team_visible = TRUE`, `team_shared_with = <team>`, and
   `team_shared_with_id = <teams.id>` under the team advisory lock
   (`lockTeamNamesTx`). By default, every member of that team (verified by both
   name and `teams.id`) is permitted to participate unless listed in
   `multiplayer_excluded`.
2. If the owner has no team (`users.team_id == ''`), any authenticated user on
   the Fleet instance can participate unless listed in `multiplayer_excluded`.
3. Specific users listed in `multiplayer_participants` can participate even if
   they are outside the owner's team, and anyone listed in
   `multiplayer_excluded` is denied multiplayer access.
4. Turning Multiplayer mode off (`multiplayer = FALSE`) clears `team_visible`,
   `team_shared_with`, and `team_shared_with_id` unless the conversation is
   currently filed in a project that is itself shared with that same team.
5. Team/project revocation lifecycle paths (leaving or changing teams, deleting
   a user, unsharing or deleting a project, moving a conversation out of a
   shared project) clear `multiplayer = FALSE` alongside `team_visible = FALSE`
   so revoking a team stamp never widens access to the whole instance.

### Per-turn sender identity & isolation

When an allowed participant submits `POST /chat` on a multiplayer conversation:

- **Turn identity (`user`):** The turn runs under the **submitting user's**
  email (`userFromCtx`), not the conversation owner's:
  - `${FLEET_USER_EMAIL}` (`MCPUserIdentity`), remote MCP overlay connections,
    and bundled connector credential seats resolve against the sender's own
    account and preferences (`effectiveOptionalMCPServersForCaller` intersects
    the conversation's enabled optional connectors with the sender's
    `optionalServerWhitelist`).
  - Personal memories (`ListMemories`), user skills (`materializeUserSkills`),
    and user persona layers resolve for the sender without exposing the owner's
    private persona instructions or memories.
  - Approval cards (`newTurnStager`), memory proposals, and input-queue rows are
    scoped to the sender.
- **Steer isolation:** Mid-turn `mode: "steer"` submissions are injected into a
  running turn's `steerMailbox` only when the submitting user matches the active
  turn's sender (`strings.EqualFold(entry.steer.user, user)`). Steers submitted
  by another participant remain queued and drain as their own turn under that
  participant's identity.
- **Transcript & SSE attribution:** Both initial and steered user entries carry
  `sender_email` (`TextContent.SenderEmail` and `user.message` SSE frames), and
  the model's injected turn context notes `[Multiplayer chat: message sent by <email>]`
  so the assistant knows which teammate is speaking.
- **Participant redaction:** Non-owner participants never receive the owner's
  `optional_mcp_servers_enabled`, `mcp_accounts`, `labels`, `share_token`,
  `parent_conversation_id`, or `auto_approve` on `store.Conversation` JSON
  responses, and another sender's `injected_context` (attachment paths,
  workspace inventory, shared library listing) is stripped from `GET /conversations/{id}`,
  `GET /conversations/{id}/export`, `GET /conversations/{id}/stream`, and
  `GET /conversations/{id}/events`. Live and replayed `queue.updated` SSE frames
  are filtered per viewer so one participant's queued drafts never overwrite
  another participant's visible queue.

### Per-instance default (`team_chats_shared_by_default`)

Operators can enable **Multiplayer team chats by default** via
`FLEET_TEAM_CHATS_SHARED_BY_DEFAULT=true` or live in **Settings → Admin → Features**
(`team_chats_shared_by_default`). When enabled:

- Every new unfiled or team-project conversation (outside Lockdown mode) created
  by a user with a non-empty `team_id` automatically starts with
  `multiplayer = TRUE` and `team_visible = TRUE`.
- Conversations created by teamless users or inside a private personal project
  remain private by default until the owner explicitly turns Multiplayer mode on
  in the Share dialog.
- Owners can still turn Multiplayer mode off or remove specific participants on
  any individual chat.

### Web UI & API surface

- `GET /conversations?scope=shared`: lists conversations shared with the caller
  via Multiplayer mode or team visibility, powering the **Team chats** sidebar
  section (with a **Live** badge on multiplayer threads).
- `GET /conversations/{id}/multiplayer` & `POST /conversations/{id}/multiplayer`:
  returns the multiplayer state + team roster (`roster`, `participants`,
  `excluded`), and lets the conversation owner toggle `multiplayer` or add/remove
  participant emails. Non-owners are rejected with `403 Forbidden` on all
  mutations.
- `GET /conversations/{id}/team-link`: returns `status: "multiplayer"` when a
  shared link points to an active multiplayer conversation the caller can join,
  routing `/chat/shared/{id}` straight into `/chat?c={id}` instead of the
  read-only branch view.

## Shipped scope and deliberate deferrals

- **Shipped:** Migration `092_multiplayer_conversations.sql`, store and HTTP
  access control, owner-gated participant add/remove controls in the Share
  dialog, per-turn sender identity for MCP/memories/skills/approvals, sender
  badges on user bubbles, the **Team chats** sidebar section, and the
  per-instance `team_chats_shared_by_default` setting.
- **Deferred:** Real-time typing indicators or presence cursors for other
  viewers, per-participant role levels beyond owner vs. participant (e.g.
  co-owners allowed to mutate the roster or delete the conversation), and
  cross-user mid-turn steer injection into another user's active turn (deliberately
  queued as a separate turn so credentials and approvals never mix within a
  single turn).
