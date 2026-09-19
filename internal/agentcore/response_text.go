package agentcore

import (
	"strings"

	"charm.land/fantasy"
)

// completedResponseText preserves every text block in the provider's completed
// response, including text on either side of non-text content.
func completedResponseText(content fantasy.ResponseContent) string {
	var text strings.Builder
	for _, part := range content {
		if block, ok := part.(fantasy.TextContent); ok {
			text.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(text.String())
}
