# Per-conversation auto-approve and 24-hour default approval timeout

This note records the design and scope of the per-conversation Auto-approve
toggle (`auto_approve`) and the 24-hour default approval expiry window.

## Problem

Traders and operators frequently run several multi-minute agent turns in
parallel across tabs. With the previous short approval window (3600s default,
and shorter overrides on some deployments), stepping away while a turn worked
caused critical-tool approval cards to expire with `timed_out:default_deny`
before the user returned. In addition, users running iterative creative or data
workflows (such as building a Bento page or iterating on datasets) wanted a
per-chat way to pre-approve standard executable critical tools for that
conversation without turning off guardrails workspace-wide or bypassing
high-consequence gates.

## What shipped

1. **24-hour default approval timeout (7-day per-conversation ceiling)**
   - `FLEET_APPROVAL_TIMEOUT_SECONDS` (and `defaultApprovalTimeoutSeconds` in
     `internal/httpapi/approvals.go`) now defaults to `86400` seconds (24 hours)
     instead of `3600` seconds.
   - The per-conversation override ceiling (`maxApprovalTimeoutSeconds` on
     `POST /conversations/{id}/approval-timeout` and
     `UpdateConversationApprovalTimeout` in `internal/store`) is now `604800`
     seconds (7 days). Operator-managed `FLEET_APPROVAL_TIMEOUT_SECONDS` and
     bundle `critical_tool_timeouts` retain their existing positive-integer
     semantics so operators can configure shorter or longer values when needed.
   - Task-staging cards (`schedule_task`, `manage_tasks`) retain a 24-hour floor
     (`stagedTaskApprovalTimeoutSeconds = 86400`) and also inherit any longer
     global or per-conversation timeout override.

2. **Per-conversation `auto_approve` and `approval_mode` persistence and API**
   - Stored on `conversations.auto_approve` (`BOOLEAN NOT NULL DEFAULT FALSE`,
     migration `internal/migrations/sql/legacy/chat/085_conversation_auto_approve.sql`)
     and `conversations.approval_mode` (`TEXT NOT NULL DEFAULT 'manual'`,
     values `'manual' | 'auto' | 'autonomous'`, migration
     `internal/migrations/sql/0200_conversation_approval_mode.up.sql`), kept in
     lockstep (`auto_approve` is `true` for `'auto'` and `'autonomous'`, `false`
     for `'manual'`).
   - Returned on `Conversation` payloads (`GET /conversations`,
     `GET /conversations/{id}`, and the `conversation` SSE frame) for the
     conversation owner, and redacted for non-owners.
   - Can be seeded on `POST /chat` or `POST /conversations`
     (`approval_mode` or `auto_approve`) and updated at any time by the
     conversation owner via `POST /conversations/{id}/auto-approve` (forwarded
     by the web gateway at `POST /api/conversations/{id}/auto-approve`).

3. **Three-tier safety boundary in `approvalStager.Stage`**
   - `Stage` re-reads the conversation row (`liveApprovalMode`) before each
     critical tool call so changing the approval mode mid-turn takes effect
     immediately on the next tool call. Non-owner turns in shared/multiplayer
     conversations always fall back to `manual`.
   - **Manual (`manual`)**: every critical tool stages an inline approval card.
   - **Auto-approve (`auto`)**: applies to executable critical tools eligible
     for session pre-approval (`SessionApprovalAllowed(toolName)`), emitting
     `tool.auto_resolved` (`via: "conversation_auto_approve"`).
   - **Autonomous (`autonomous`)**: requires server-enforced risk
     acceptance (`chat.autonomous_acceptances`, `POST /api/chat/autonomous-accept`,
     `CurrentAutonomousPolicyVersion = 1`), and switching a chat or Launchpad
     patrol into `Autonomous` prompts the confirmation dialog (`AutonomousRiskModal`)
     each time the user enters `Autonomous` mode. In owner-initiated turns and owner
     patrols, default-allows bundle-configured critical tools and
     `critical_tool_no_session_approval` tools (plus inline
     `enable_mcp_connectors`, self-addressed owner emails, and eligible
     prefilled `show_ui` forms after a 10-second server-owned countdown in
     `chat.ui_autosubmits`), persisting an approved audit record in
     `chat.approvals` (`"Ran without asking: this chat is in Autonomous mode."`),
     rendering a muted receipt card (`tool.action_recorded`), and attaching the
     tool's structured `result_card` once execution finishes.
   - **Always excluded even in `Autonomous` mode** (still stage an explicit card):
     - Bundle-blocked suffixes configured in `agent_policy.critical_tool_autonomous_blocked`
       (for example, destructive resource-deletion or external vendor-messaging
       tools declared by a deployment bundle).
     - External outbound messages and shared email templates: non-self
       `send_email` / `send_template_email`, `save_email_template` /
       `delete_email_template`, and Microsoft 365
       mail/chat/channel/calendar/sharing/rules/batch tools.
     - Privilege-persistence & untrusted-server floor: `schedule_task`,
       `manage_tasks`, `chat_patrol` `create`/`update`, `connector_studio`
       mutating/probe actions, and user-connected dynamic remote MCP tools
       (which keep `auto` mode gating).
     - Multiplexed destructive storage/sharing (`mcp_fast_io_share` and
       `mcp_fast_io_storage` `delete`/`purge`/`trash`/`remove`) and `git push`.
   - Explicit session-level deny policies (`SessionApprovalRegistry` `mode:
     "deny"`) are evaluated before `auto_approve` and always take precedence.

4. **Composer toggle UI**
   - A compact `Auto-approve` (`⚡`) button in the chat composer toolbar lets
     the owner toggle `auto_approve` before starting a new chat or at any point
     during an existing conversation.
   - Optimistic toggles are serialized per conversation and reconciled against
     concurrent conversation-list refreshes so out-of-order responses never
     desynchronize the displayed toggle from the persisted database state.

## Deviations and deferred work

- **No workspace-wide or user-default auto-approve setting:** Auto-approve is
  deliberately scoped to a single conversation (`conversations.auto_approve`)
  and resets to `false` on every new chat so a user always opts in explicitly
  for the specific chat they want to run hands-off.
