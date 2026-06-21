package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// TavilySearchTool performs web searches using Tavily API (designed for AI agents)
type TavilySearchTool struct {
	apiKey      string
	client      *http.Client
	rateLimiter *rateLimiter
}

// TavilySearchRequest represents a Tavily API search request
type TavilySearchRequest struct {
	AuthToken         string   `json:"api_key"`
	Query             string   `json:"query"`
	SearchDepth       string   `json:"search_depth,omitempty"`        // "basic" or "advanced"
	MaxResults        int      `json:"max_results,omitempty"`         // Default: 5
	IncludeAnswer     bool     `json:"include_answer,omitempty"`      // Include AI-generated answer
	IncludeRawContent bool     `json:"include_raw_content,omitempty"` // Include full page content
	IncludeDomains    []string `json:"include_domains,omitempty"`
	ExcludeDomains    []string `json:"exclude_domains,omitempty"`
}

// TavilySearchResponse represents a Tavily API search response
type TavilySearchResponse struct {
	Answer       string               `json:"answer,omitempty"`
	Query        string               `json:"query"`
	Results      []TavilySearchResult `json:"results"`
	ResponseTime float64              `json:"response_time"`
}

// TavilySearchResult represents a single search result from Tavily
type TavilySearchResult struct {
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
	RawContent string  `json:"raw_content,omitempty"`
}

// NewTavilySearchTool creates a new Tavily search tool
func NewTavilySearchTool() *TavilySearchTool {
	apiKey := os.Getenv("TAVILY_API_KEY")

	return &TavilySearchTool{
		apiKey: apiKey,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		rateLimiter: newRateLimiter(1 * time.Second), // 1 second between requests
	}
}

func (t *TavilySearchTool) Run(ctx context.Context, args map[string]interface{}) (string, error) {
	// Check if API key is configured
	if t.apiKey == "" {
		return "", errors.New("TAVILY_API_KEY environment variable not set. Get a free API key at https://tavily.com")
	}

	query, ok := args["query"].(string)
	if !ok || query == "" {
		return "", errors.New("query parameter is required and must be a string")
	}

	// Parse optional parameters
	maxResults := 5
	if mr, ok := args["max_results"].(float64); ok {
		maxResults = int(mr)
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 10 {
		maxResults = 10
	}

	searchDepth := "basic"
	if sd, ok := args["search_depth"].(string); ok && (sd == "basic" || sd == "advanced") {
		searchDepth = sd
	}

	includeAnswer := true
	if ia, ok := args["include_answer"].(bool); ok {
		includeAnswer = ia
	}

	// Apply rate limiting
	if err := t.rateLimiter.wait(ctx, "tavily"); err != nil {
		return "", fmt.Errorf("rate limit wait cancelled: %w", err)
	}

	// Perform search
	response, err := t.search(ctx, query, maxResults, searchDepth, includeAnswer)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	return t.formatResults(response), nil
}

func (t *TavilySearchTool) search(ctx context.Context, query string, maxResults int, searchDepth string, includeAnswer bool) (*TavilySearchResponse, error) {
	payload := map[string]interface{}{
		"api_key":        t.apiKey,
		"query":          query,
		"search_depth":   searchDepth,
		"max_results":    maxResults,
		"include_answer": includeAnswer,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("search failed with status %d: %s", resp.StatusCode, string(body))
	}

	var searchResp TavilySearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &searchResp, nil
}

func (t *TavilySearchTool) formatResults(response *TavilySearchResponse) string {
	var result bytes.Buffer

	// Include AI-generated answer if available
	if response.Answer != "" {
		result.WriteString("AI Answer:\n")
		result.WriteString(response.Answer)
		result.WriteString("\n\n")
	}

	// Format search results
	if len(response.Results) == 0 {
		result.WriteString("No results found for your query.")
		return result.String()
	}

	fmt.Fprintf(&result, "Found %d search results:\n\n", len(response.Results))

	for i, res := range response.Results {
		fmt.Fprintf(&result, "%d. %s\n", i+1, res.Title)
		fmt.Fprintf(&result, "   URL: %s\n", res.URL)
		fmt.Fprintf(&result, "   Relevance: %.2f\n", res.Score)
		fmt.Fprintf(&result, "   Summary: %s\n\n", res.Content)
	}

	fmt.Fprintf(&result, "(Search completed in %.2fs)\n", response.ResponseTime)

	return result.String()
}
