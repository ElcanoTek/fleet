package tools

import (
	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// TurnTools is a bundle of native tools for a single agent turn.
// Bash and run_python are bound to a per-turn sandbox container; call
// Cleanup at turn end to tear it down.
type TurnTools struct {
	Tools   []fantasy.AgentTool
	Cleanup func()
}

// interactiveOnlyToolNames lists the native tools whose ENTIRE behavior is an
// interactive staging card: the orchestration guard in internal/agent
// intercepts each call and stages an approval/preview/proposal for the human
// at the keyboard. Their raw Run is either a deliberate mis-wiring tripwire
// (preview_email, schedule_task, suggest_advanced_model return a NON-NIL Go
// error — fatal to the whole agent loop) or a fake success that goes nowhere
// (propose_memory). A headless scheduled run has neither the interceptor nor
// a user to review a card, so these tools must not be offered there at all:
// a scheduled model that called preview_email to present its report killed
// its entire run as a non-retryable failure.
var interactiveOnlyToolNames = map[string]bool{
	"preview_email":              true,
	ScheduleTaskToolName:         true,
	ManageTasksToolName:          true,
	SuggestAdvancedModelToolName: true,
	"propose_memory":             true,
}

// ExcludeInteractiveOnly returns the tools minus the interactive-staging-card
// set above. The scheduled driver applies it to the shared turn bundle so the
// interactive roster (and its prompt-prefix byte stability) is untouched.
func ExcludeInteractiveOnly(all []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, 0, len(all))
	for _, t := range all {
		if interactiveOnlyToolNames[t.Info().Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// DefaultTools returns the stateless native-tool set, plus the
// sandbox-bound entries bound to a nil sandbox. Those surface a
// clear "no sandbox" error if ever invoked through this slice —
// production turns rebuild via [NewTurnTools] with a real per-turn
// sandbox, and that's the only path that should fire them.
// The nil-bound entries here (bash, run_python, view/write/edit_file —
// sandbox data-plane since #784 — and download_url, xlsx_workbook,
// generate_image, whose file I/O moved into the sandbox FileOp seam in
// #1083) exist so the tool *schemas* (name, description, parameters)
// stay stable for the system prompt and prompt-prefix caching, even
// before the agent has Take()d a sandbox for the turn. Invoked with a
// nil sandbox they fail closed — there is no host-execution fallback.
func DefaultTools() []fantasy.AgentTool {
	return []fantasy.AgentTool{
		NewBashTool(nil),
		NewViewFileTool(nil),
		NewWriteFileTool(nil),
		NewEditFileTool(nil),
		NewTaskTrackerTool(),
		NewWebFetchTool(),
		NewDownloadURLTool(nil),
		NewSmartSearchTool(),
		NewPreviewEmailTool(),
		NewScheduleTaskTool(),
		NewManageTasksTool(),
		NewSuggestAdvancedModelTool(),
		NewXLSXTool(nil),
		NewProposeMemoryTool(),
		NewRunPythonTool(nil),
		NewGenerateImageTool(nil),
	}
}

// NewTurnTools constructs the per-turn tool bundle, with bash and
// run_python both bound to the supplied sandbox. Cleanup tears down
// the sandbox (and with it the python kernel and any in-flight bash
// state) when the turn ends.
//
// The #191 git-metadata tools are deliberately NOT added here. They are wired
// only into the scheduled native set (where code-producing agents live and the
// per-task MCP selection is narrow), not the interactive chat turn — which runs
// near the 128-tool ceiling once per-user MCP servers (#449) load — via
// [MetadataTools]. See internal/scheduledrun.
func NewTurnTools(sb *sandbox.Sandbox) TurnTools {
	return TurnTools{
		Tools: []fantasy.AgentTool{
			NewBashTool(sb),
			NewViewFileTool(sb),
			NewWriteFileTool(sb),
			NewEditFileTool(sb),
			NewTaskTrackerTool(),
			NewWebFetchTool(),
			NewDownloadURLTool(sb),
			NewSmartSearchTool(),
			NewPreviewEmailTool(),
			NewScheduleTaskTool(),
			NewManageTasksTool(),
			NewSuggestAdvancedModelTool(),
			NewXLSXTool(sb),
			NewProposeMemoryTool(),
			NewRunPythonTool(sb),
			NewGenerateImageTool(sb),
		},
		Cleanup: sb.Close,
	}
}
