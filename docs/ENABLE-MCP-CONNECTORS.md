# In-Chat Optional MCP Connector Enablement (`enable_mcp_connectors`)

## Context & Problem

Interactive chat conversations start with:
1. All **Always-On** MCP connectors mounted (`pages`, `explorer`, `lens`, etc.).
2. Only the **Optional** MCP connectors the user has enabled by default (under **Settings → Connections**) or toggled on in the conversation's **Tools** picker.

Previously, when a user asked for something handled by a connected Optional MCP connector that was not yet enabled in the current conversation, the agent had no visibility into which optional connectors were available and no way to turn them on. As a result, the model would flail — searching the filesystem, probing environment variables with `bash`, or guessing CLI invocations.

## What Shipped

### 1. System Prompt Catalog (`Available Optional MCP Connectors`)

During interactive chat turns where an `enable_mcp_connectors` handler is wired (`tools.HasEnableMCPConnectorsHandler(ctx)`):
- `Server.availableOptionalConnectorsForUser` builds the user's connected optional connector catalog by combining:
  - **Bundled optional MCP servers** (`MCPServerCatalog()`) whose live discovery status is available (`ToolCount > 0`) and that are not disabled in the user's `ConnectorPrefs`.
  - **Connected remote (hosted) MCP servers** (`remoteMCP.ConnectedServersForUser`) for the user.
- `buildSystemPrompt` (`internal/agent/prompt.go`) renders an **`Available Optional MCP Connectors (Connected — Not Yet Enabled in This Chat)`** section listing connectors that are not yet in `OptionalMCPServersEnabled` and that have at least one tool permitted by `interactive_hidden_tools` and the active persona policy.
- **Prompt-cache & prompt-injection invariants**:
  - Entries are sorted deterministically by canonical name (and tool names sorted alphabetically, capped at 12 previewed tool names per server and `MaxAvailableOptionalInPrompt = 25` servers total) to uphold `docs/PROMPT-CACHE-CONTRACT.md`.
  - User- or sharer-controlled remote MCP display labels and descriptions are omitted from prompt assembly (`Remote: true` renders only the sanitized `promptSafeName`), and bundled display names/descriptions are clamped via `clampPromptRunes`.
  - Remote MCP server names are bounded to `MaxRemoteMCPNameRunes = 64` runes at registration time (`internal/remotemcp/service.go`).
  - Turn callers that do not wire an `enable_mcp_connectors` handler (scheduled jobs, dataset runs, eval harnesses) neither advertise `enable_mcp_connectors` in the tool roster nor render the prompt section.

### 2. `enable_mcp_connectors` Tool & Validation

- Defined in `internal/tools/enable_mcp_connectors.go` (`enable_mcp_connectors`) and wired per interactive turn via `tools.WithEnableMCPConnectorsHandler`.
- `Server.validateEnableMCPConnectors` (`internal/httpapi/enable_mcp_connectors.go`) validates each requested connector name (accepting either the stored lowercase name or its `agentcore.PromptSafeName` identifier):
  - Rejects unknown connectors, unconfigured (`needs_setup`) connectors, 0-tool unavailable connectors, connectors disabled in the user's **Settings → Connections** preferences, disconnected remote connectors, and connectors blocked by `interactive_hidden_tools` or persona tool permissions.
  - Treats already-enabled or always-on connectors idempotently.
- Per-conversation updates are serialized via `Server.connectorLocks` (`keyedLocks`).

### 3. User-Approval Flow vs. Auto-Approve Flow

- **Standard (User Approval) Mode**:
  - Stages a handler-only `enable_mcp_connectors` approval row (` SupersedePendingApprovals` supersedes any older pending card in the conversation) and arms `ApprovalResumeArmed`.
  - Emits `tool.approval_required` to the live stream and returns `Status: "approval_required"` instructing the model to explain why the connector is needed and wait.
  - When the user clicks **Approve & continue** on `EnableMCPConnectorsCard` (`web/src/app/chat/ui/ApprovalCards.tsx`), `runStagedEnableMCPConnectors` re-validates and persists `OptionalMCPServersEnabled`, returns `enabled_optional` to update the client's Tools picker state immediately, and `decideApprovalResume` launches the continuation turn with the newly enabled MCP connectors mounted.
- **Auto-Approve Mode** (conversation **⚡ Auto-approve**, unattended in-chat patrol turns, or `autoApproveInTest`):
  - Creates, arms, and resolves an `approved` audit approval row first (failing closed if any persistence step errors), persists `OptionalMCPServersEnabled`, emits `mcp.servers_updated` and `tool.action_recorded`, claims the armed resume via `decideApprovalResume(conv.ID, 0)`, and returns `StopTurn: true` (`agentcore.ToolOutput{StopTurn: true}`).
  - `agentcore` halts the current step cleanly after committing the tool result, and `runTurnAsync`'s completion tail drains the queued `resume` turn immediately with the newly enabled connectors mounted.

## Deferred / Non-Goals

- **Disabling connectors from chat**: Users can turn connectors off at any time from the **Tools** picker; the agent does not have a tool to disable connectors.
- **Unattended scheduled tasks / dataset runs**: Scheduled tasks use the connector binding fixed on the task definition in the Operations Center rather than mutating connectors mid-run.
