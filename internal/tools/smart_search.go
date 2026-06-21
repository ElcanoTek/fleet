package tools

import (
	"context"
	"fmt"
	"os"

	"charm.land/fantasy"
)

// SmartSearchParams are the typed parameters for the web_search tool.
type SmartSearchParams struct {
	Query       string `json:"query" description:"The search query to find information on the web."`
	MaxResults  int    `json:"max_results,omitempty" description:"Maximum number of results to return (default: 10 for DuckDuckGo, 5 for Tavily)."`
	ForceTavily bool   `json:"force_tavily,omitempty" description:"Force use of Tavily API instead of trying DuckDuckGo first (requires TAVILY_API_KEY)."`
}

// NewSmartSearchTool creates a fantasy.AgentTool that intelligently chooses between
// DuckDuckGo (free) and Tavily (API key) for web searches.
func NewSmartSearchTool() fantasy.AgentTool {
	duckduckgo := NewWebSearchTool()
	tavily := NewTavilySearchTool()

	hasTavily := os.Getenv("TAVILY_API_KEY") != ""
	description := "Search the web using DuckDuckGo. Returns search results with titles, URLs, and snippets."
	if hasTavily {
		description = "Search the web intelligently. Tries DuckDuckGo first (free), automatically falls back to Tavily API (AI-optimized) if blocked."
	}

	return fantasy.NewAgentTool("web_search", description,
		func(ctx context.Context, params SmartSearchParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			hasTavilyNow := os.Getenv("TAVILY_API_KEY") != ""

			// Build args map for internal delegation
			args := map[string]interface{}{"query": params.Query}
			if params.MaxResults > 0 {
				args["max_results"] = float64(params.MaxResults)
			}

			if params.ForceTavily && hasTavilyNow {
				result, err := tavily.Run(ctx, args)
				if err == nil {
					return fantasy.NewTextResponse(result), nil
				}
				result, err = duckduckgo.Run(ctx, args)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				return fantasy.NewTextResponse(result), nil
			}

			result, err := duckduckgo.Run(ctx, args)
			if err == nil && !isDuckDuckGoBlocked(result) {
				return fantasy.NewTextResponse(result), nil
			}

			if hasTavilyNow && isDuckDuckGoBlocked(result) {
				tavilyResult, tavilyErr := tavily.Run(ctx, args)
				if tavilyErr == nil {
					return fantasy.NewTextResponse(fmt.Sprintf("(DuckDuckGo blocked, using Tavily)\n\n%s", tavilyResult)), nil
				}
			}

			if isDuckDuckGoBlocked(result) && !hasTavilyNow {
				return fantasy.NewTextResponse(result + "\n\nTip: For more reliable searches, set TAVILY_API_KEY environment variable"), nil
			}

			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

func isDuckDuckGoBlocked(result string) bool {
	return len(result) > 0 && (len(result) >= 20 && result[:20] == "No results were foun" ||
		len(result) < 50)
}
