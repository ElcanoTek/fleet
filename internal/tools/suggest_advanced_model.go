package tools

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// SuggestAdvancedModelToolName is the canonical tool name. Exported so
// orchestration and the approval handler can reference it without
// re-typing the string.
const SuggestAdvancedModelToolName = "suggest_advanced_model"

// SuggestAdvancedModelParams carries the agent-facing payload — a
// single one-line `reason` shown to the user on the inline approval
// card. The runtime intercepts every call, stages a suggestion record,
// and returns a SUGGESTION_DISPLAYED blocking response. The user picks
// Switch & retry / Just switch / Dismiss in the UI.
type SuggestAdvancedModelParams struct {
	Reason string `json:"reason" description:"One short sentence the user will see on the suggestion card, explaining why advanced mode would help here. Keep it concrete (e.g. 'I'm uncertain about the CPDPV formula for this client and would cross-reference better in advanced mode.'). Required."`
}

const suggestAdvancedModelDescription = `Recommend the user switch this conversation to the ADVANCED model for the rest of the session. The runtime renders an inline card with three actions — Switch & retry (default), Just switch, Dismiss — and the user picks. You do NOT switch the model yourself; you only stage the suggestion.

When to call:
- You've hit a domain-specific KPI / formula / business rule you're uncertain about and the answer matters (a client's primary KPI in a tracking sheet, a contract-specific margin definition, etc.).
- The user has corrected you twice on the same point and you suspect the issue is depth of reasoning, not a formatting/presentation glitch.
- The workload is known to push against your context limits (large workbook + multi-tab analysis, very long historical thread).

When NOT to call:
- Routine questions you can answer directly.
- The user's first correction — try once before escalating; a single fix is rarely a stuckness signal.
- After the user has dismissed a previous suggestion in this conversation. The runtime gates re-suggestions automatically (cool-down by user turns); respect the gate and don't try to bypass it. If suppressed, a SUGGESTION_SUPPRESSED message comes back — do NOT retry the call, just keep working.

Pass a single one-line user-facing reason. After calling, briefly summarize what you've already done in this turn and stop iterating — wait for the user to act on the card.`

// NewSuggestAdvancedModelTool returns the suggest_advanced_model tool.
// Run errors loudly — orchestration must intercept before this fires.
// Mirrors the preview_email pattern: the tool's value is the staged
// approval card the runtime emits, not anything the tool itself does.
func NewSuggestAdvancedModelTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(SuggestAdvancedModelToolName, suggestAdvancedModelDescription,
		func(_ context.Context, _ SuggestAdvancedModelParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextErrorResponse(
				"suggest_advanced_model was executed directly — orchestration should have staged it for the UI. This is a bug.",
			), fmt.Errorf("suggest_advanced_model bypass")
		})
}
