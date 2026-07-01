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

// DefaultTools returns the stateless native-tool set, plus bash and
// run_python entries bound to a nil sandbox. Those two surface a
// clear "no sandbox" error if ever invoked through this slice —
// production turns rebuild via [NewTurnTools] with a real per-turn
// sandbox, and that's the only path that should fire bash/run_python.
// The nil-bound entries here exist so the tool *schemas* (name,
// description, parameters) stay stable for the system prompt and
// prompt-prefix caching, even before the agent has Take()d a
// sandbox for the turn.
func DefaultTools() []fantasy.AgentTool {
	return []fantasy.AgentTool{
		NewBashTool(nil),
		NewViewFileTool(),
		NewWriteFileTool(),
		NewEditFileTool(),
		NewTaskTrackerTool(),
		NewWebFetchTool(),
		NewDownloadURLTool(),
		NewSmartSearchTool(),
		NewPreviewEmailTool(),
		NewScheduleTaskTool(),
		NewSuggestAdvancedModelTool(),
		NewXLSXTool(),
		NewProposeMemoryTool(),
		NewRunPythonTool(nil),
		NewGenerateImageTool(),
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
// TurnOption tunes the interactive turn tool set (#503 browser gating, etc.).
type TurnOption func(*turnOptions)

type turnOptions struct {
	browser BrowserConfig
}

// WithBrowser registers the governed browser tool (#503) when cfg.Enabled.
func WithBrowser(cfg BrowserConfig) TurnOption {
	return func(o *turnOptions) { o.browser = cfg }
}

func NewTurnTools(sb *sandbox.Sandbox, opts ...TurnOption) TurnTools {
	var o turnOptions
	for _, opt := range opts {
		opt(&o)
	}
	tt := TurnTools{
		Tools: []fantasy.AgentTool{
			NewBashTool(sb),
			NewViewFileTool(),
			NewWriteFileTool(),
			NewEditFileTool(),
			NewTaskTrackerTool(),
			NewWebFetchTool(),
			NewDownloadURLTool(),
			NewSmartSearchTool(),
			NewPreviewEmailTool(),
			NewScheduleTaskTool(),
			NewSuggestAdvancedModelTool(),
			NewXLSXTool(),
			NewProposeMemoryTool(),
			NewRunPythonTool(sb),
			NewGenerateImageTool(),
		},
		Cleanup: sb.Close,
	}
	// Governed browser (#503): interactive-only, opt-in per deployment, and
	// registered even in lockdown so the model gets a clear per-call refusal
	// (the tool itself errors) rather than the capability silently vanishing.
	if o.browser.Enabled {
		tt.Tools = append(tt.Tools, NewBrowserTool(sb, o.browser))
	}
	return tt
}
