package tools

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// ProposeMemoryParams are the typed parameters for the propose_memory tool.
type ProposeMemoryParams struct {
	Content string `json:"content" description:"The memory text to propose saving. Should be concise (1-2 sentences) and capture a durable user preference, fact, or context."`
}

// NewProposeMemoryTool creates the propose_memory native tool.
// The actual proposal handling (DB persistence + SSE emission) is done in the
// orchestration layer via MemoryProposer, so this tool just returns a clean
// result the model can reference in its reply.
func NewProposeMemoryTool() fantasy.AgentTool {
	description := `Propose saving a durable memory for this user.

Use this tool SPARINGLY — only when the user shares a preference, fact, or context that should persist across ALL future conversations. Good candidates:
- "I prefer short, direct answers"
- "My trading philosophy is X"
- "Kyle's tracking doc uses naming convention Y"
- "I work in timezone Z"

Do NOT use for:
- Temporary or session-specific information
- Things the user is just mentioning in passing
- Factual questions the user asks (they're not telling you to remember it)

When you call this tool, the user will see an inline card asking "Save this memory?" with Save/Don't Save buttons. Only call it when you're confident the user WANTS this remembered long-term.`

	return fantasy.NewAgentTool("propose_memory", description,
		func(_ context.Context, params ProposeMemoryParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			content := params.Content
			if content == "" {
				return fantasy.NewTextErrorResponse("Memory content cannot be empty."), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Memory proposal created: %s", content)), nil
		})
}
