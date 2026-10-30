# In-Chat Optional MCP Connector Enablement (`enable_mcp_connectors`)

## Context & Problem

Interactive chat conversations start with:
1. All **Always-On** MCP connectors mounted (`pages`, `explorer`, `lens`, etc.).
2. Only the **Optional** MCP connectors the user has enabled by default (under **Settings → Connections**) or toggled on in the conversation's **Tools** picker.

Previously, when a user asked for something handled by a connected Optional MCP connector that was not yet enabled in the current conversation, the agent had no visibility into which optional connectors were available and no way to turn them on. As a result, the model would flail — searching the filesystem, probing environment variables with `bash`, or guessing CLI invocations.

## What Shipped

### 1. System Prompt Catalog (`Available Optional MCP Connectors`)

During interactive chat turns where an `enable_mcp_connectors` handler is wired (`tools.HasEnableMCPConnectorsHandler(ctx)`):
- `Server.availableOptionalConnectorsForUser` builds the user's connected bundled optional connector catalog from **bundled optional MCP servers** (`MCPServerCatalog()`, excluding the synthetic `image_generation` toggle and third-party remote MCP connections) whose live discovery status is available (`ToolCount > 0`) and that are not disabled in the user's `ConnectorPrefs` (failing closed if preferences cannot be loaded).
- `buildSystemPrompt` (`internal/agent/prompt.go`) renders an **`Available Optional MCP Connectors (not currently enabled)`** section listing connectors that are not yet in `OptionalMCPServersEnabled` and that have at least one tool permitted by `interactive_hidden_tools` and the active persona policy (checked against the runtime tool name `agentcore.MCPToolName`).
- **Prompt-cache & prompt-injection invariants**:
  - Entries are sorted deterministically by canonical `promptSafeName`, showing the tool count (e.g. `3 tools`) and capped at `MaxAvailableOptionalInPrompt = 25` servers total to uphold `docs/PROMPT-CACHE-CONTRACT.md`.
  - User- or sharer-controlled remote MCP display labels and descriptions are omitted from prompt assembly (`Remote: true` renders only the sanitized `promptSafeName`), and bundled display names (`maxOptionalConnectorLabelRunes = 64`) and descriptions (`maxOptionalConnectorDescRunes = 200`) are clamped via `clampPromptRunes`.
  - If two distinct connectors map to the same `promptSafeName`, the ambiguous alias is omitted from the prompt roster and refused by `validateEnableMCPConnectors`.
  - Every model-visible tool result and error string uses `agentcore.PromptSafeName` so untrusted remote connector labels never enter model context.
  - Turn callers that do not wire an `enable_mcp_connectors` handler (scheduled jobs, dataset runs, eval harnesses) neither advertise `enable_mcp_connectors` in the tool roster nor render the prompt section.

### 2. `enable_mcp_connectors` Tool & Validation

- Defined in `internal/tools/enable_mcp_connectors.go` (`enable_mcp_connectors`) and wired per interactive turn via `tools.WithEnableMCPConnectorsHandler`.
- `Server.validateEnableMCPConnectors` (`internal/httpapi/enable_mcp_connectors.go`) validates each requested connector name (accepting either the stored lowercase name or its `agentcore.PromptSafeName` identifier):
  - Rejects unknown connectors, unconfigured (`needs_setup`) connectors, 0-tool unavailable connectors, connectors disabled in the user's **Settings → Connections** preferences, third-party remote connectors, the native `image_generation` toggle, and connectors blocked by `interactive_hidden_tools` or persona tool permissions.
  - Treats already-enabled or always-on connectors idempotently.
- Per-conversation updates are serialized via `Server.connectorLocks` (`keyedLocks`).

### 3. User-Approval Flow vs. Auto-Approve Flow

- **Standard (User Approval) Mode**:
  - Calls `store.StageEnableMCPConnectorsApproval`, which in a single database transaction locks the conversation, supersedes any older pending `enable_mcp_connectors` card, and inserts a new pending `enable_mcp_connectors` approval row directly with `resume_state = 'armed'`.
  - Emits `tool.approval_required` to the live stream and returns `Status: "approval_required"` instructing the model to explain why the connector is needed and wait.
  - When the user clicks **Approve & continue** on `EnableMCPConnectorsCard` (`web/src/app/chat/ui/ApprovalCards.tsx`), `runStagedEnableMCPConnectors` resolves the conversation's effective bundle persona (`resolveTurnPersona` + `PersonaPolicy`), re-validates against the 25-connector prompt window and user preferences, persists `OptionalMCPServersEnabled`, returns `enabled_optional` to update the client's Tools picker state immediately, and `decideApprovalResume` launches the continuation turn with the newly enabled MCP connectors mounted.
- **Auto-Approve Mode** (conversation **⚡ Auto-approve** or `autoApproveInTest`):
  - Calls `store.AutoApproveEnableMCPConnectors`, which in a single database transaction locks the conversation, supersedes any older pending `enable_mcp_connectors` card, persists `OptionalMCPServersEnabled`, inserts the approval audit row (`status = 'approved'`, `is_err = FALSE`), and — when no other approval card is pending/executing and the conversation is within its hourly auto-resume cap and input-queue limit — atomically enqueues the `'resume'` input row (`resume_state = 'claimed'`) in the same transaction. When the continuation cannot be enqueued immediately, the audit row is committed with `resume_state = 'skipped'` (never `'armed'`) so no asynchronous retry or later settlement can launch a duplicate resume turn.
  - When `AutoApproveEnableMCPConnectors` commits a `'resume'` input row (`ResumeInput != nil`), `autoApproveEnableMCPConnectors` returns `StopTurn: true` so `agentcore` halts the current step cleanly and `runTurnAsync`'s completion tail drains the already-queued `resume` turn with the newly enabled connectors mounted. When `ResumeInput == nil`, `autoApproveEnableMCPConnectors` returns `StopTurn: false` and informs the model that the connector is enabled for the next turn so the current turn can finish without stalling.

## Deferred / Non-Goals

- **Disabling connectors from chat**: Users can turn connectors off at any time from the **Tools** picker; the agent does not have a tool to disable connectors.
- **Unattended scheduled tasks / dataset runs**: Scheduled tasks use the connector binding fixed on the task definition in the Operations Center rather than mutating connectors mid-run.
