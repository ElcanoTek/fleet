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
	// show_ui is not a staging card, but its whole value is a card drawn for a
	// person at the keyboard and its reply arrives as that person's next
	// message — a headless run has neither, so offering it there would let a
	// scheduled model "ask" a form nobody will ever submit.
	ShowUIToolName:     true,
	ChatPatrolToolName: true,
	// persona_studio drafts a persona for the person at the keyboard to
	// review; a headless run has no owner reviewing in the moment.
	PersonaStudioToolName:       true,
	EnableMCPConnectorsToolName: true,
	ConnectorStudioToolName:     true,
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

// baseTools is the one native-tool roster, with the sandbox-bound entries
// (bash, run_python, view/write/edit_file — sandbox data-plane since #784 —
// and download_url, xlsx_workbook, generate_image, whose file I/O moved into
// the sandbox FileOp seam in #1083) bound to sb. With a nil sb those entries
// keep their schemas but fail closed when invoked — there is no
// host-execution fallback.
func baseTools(sb *sandbox.Sandbox) []fantasy.AgentTool {
	return []fantasy.AgentTool{
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
		NewShowUITool(),
		NewChatPatrolTool(),
		NewPersonaStudioTool(),
		NewEnableMCPConnectorsTool(),
		NewConnectorStudioTool(),
		NewPlaywrightBrowserTool(),
	}
}

// DefaultTools returns the native roster with every sandbox-bound entry bound
// to a nil sandbox: the same names and schemas a turn advertises, none of the
// sandbox-bound ones runnable (they fail closed). No production path uses it —
// turns build their tools with [NewTurnTools] — but tests here and in
// internal/agent need the roster without a sandbox, and a cross-package test
// helper has to be exported.
func DefaultTools() []fantasy.AgentTool {
	return baseTools(nil)
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
// browserbaseKey unseals the running user's Browserbase connector credential and
// is nil unless one is genuinely reachable for this turn; a non-nil value is what
// registers browserbase_live_view, with the host env key as the fallback.
func NewTurnTools(sb *sandbox.Sandbox, browserbaseKey BrowserbaseKeyFunc) TurnTools {
	turn := baseTools(sb)
	if t := NewBrowserbaseLiveViewTool(browserbaseKey); t != nil {
		turn = append(turn, t)
	}
	return TurnTools{
		Tools:   turn,
		Cleanup: sb.Close,
	}
}
