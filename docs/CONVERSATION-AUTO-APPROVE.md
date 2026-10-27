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

2. **Per-conversation `auto_approve` persistence and API**
   - Stored on `conversations.auto_approve` (`BOOLEAN NOT NULL DEFAULT FALSE`,
     migration `internal/store/migrations/045_conversation_auto_approve.sql`).
   - Returned on `Conversation` payloads (`GET /conversations`,
     `GET /conversations/{id}`, and the `conversation` SSE frame).
   - Can be seeded on the initial `POST /chat` request (`auto_approve: true`)
     when creating a conversation on first turn, and toggled at any time by the
     conversation owner via `POST /conversations/{id}/auto-approve` (forwarded
     by the web gateway at `POST /api/conversations/{id}/auto-approve`).

3. **Safety boundary in `approvalStager.Stage`**
   - When `auto_approve` is active for a conversation, `Stage` re-reads the
     conversation row (`liveAutoApprove`) before each critical tool call so
     toggling Auto-approve on or off mid-turn takes effect immediately on the
     next tool call.
   - Auto-approve applies **only** to executable critical tools that are eligible
     for session pre-approval (`!handlerOnlyApproval(toolName) &&
     !tools.IsEmailToolName(toolName) && !agentcore.IsOutboundEmailTool(toolName)
     && agentcore.SessionApprovalAllowed(toolName)`).
   - It emits `tool.auto_resolved` (`mode: "approve"`,
     `via: "conversation_auto_approve"`) and returns `PreApprovedSentinel`.
   - **Always excluded from Auto-approve** (still stage an explicit card):
     - Handler-only approval cards (`schedule_task`, `manage_tasks`,
       `preview_email`, `suggest_advanced_model`).
     - Outbound email tools (`send_email`, `send_template_email`, and any
       `mcp_*_send_email` / `mcp_*_send_template_email` tool).
     - Bundle-declared `critical_tool_no_session_approval` tools.
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
