// Package tools exposes the native (non-MCP) tools the LLM can call.
//
// Each tool is a [fantasy.AgentTool] built by a dedicated constructor:
//
//   - NewBashTool          — shell commands inside the per-turn sandbox
//     (rootless container in production, host
//     fallback for tests). Application-level
//     denylist is third-layer defense.
//   - NewViewFileTool,
//     NewWriteFileTool,
//     NewEditFileTool      — filesystem ops path-scoped by pathsec.go and
//     EXECUTED inside the per-turn sandbox via the
//     FileOp seam (#784). edit_file is unique-match /
//     stale-guarded / diff-returning (#787,
//     docs/FILE-EDIT-SAFETY.md).
//   - NewRunPythonTool     — IPython kernel hosted by the per-turn
//     sandbox. Same JSON wire shape as before;
//     implementation moved into the [sandbox]
//     package.
//   - NewTaskTrackerTool   — markdown-checklist helper
//   - NewWebFetchTool      — fetch one URL and convert to Markdown
//   - NewWebSearchTool     — DuckDuckGo fallback (no API key needed)
//   - NewSmartSearchTool   — Tavily-backed search when TAVILY_API_KEY is set
//
// Lifetime:
//
//   - fs, task_tracker, web_fetch, web_search, smart_search are
//     stateless and cheap to construct. The one roster (baseTools) is
//     built per turn by [NewTurnTools]; [DefaultTools] is the same
//     roster bound to a nil sandbox, for tests — its sandbox-bound
//     entries keep their schemas but fail closed if invoked.
//   - For production turns, use [NewTurnTools] with the per-turn
//     sandbox handed out by [sandbox.Pool.Take]. The sandbox owns the
//     IPython kernel and is torn down at turn end via Cleanup().
package tools
