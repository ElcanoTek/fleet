package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// SearchResult represents a single search result from DuckDuckGo
type SearchResult struct {
	Title    string
	Link     string
	Snippet  string
	Position int
}

// WebSearchTool performs web searches using DuckDuckGo
type WebSearchTool struct {
	client      *http.Client
	rateLimiter *rateLimiter
}

// rateLimiter implements simple rate limiting for web requests
type rateLimiter struct {
	mu            sync.Mutex
	lastRequest   time.Time
	minInterval   time.Duration
	requestCounts map[string]int
	resetTime     time.Time
}

func newRateLimiter(minInterval time.Duration) *rateLimiter {
	return &rateLimiter{
		minInterval:   minInterval,
		requestCounts: make(map[string]int),
		resetTime:     time.Now().Add(time.Minute),
	}
}

func (rl *rateLimiter) wait(ctx context.Context, key string) error {
	// The mutex is released before every blocking wait and re-acquired after.
	// `locked` tracks whether we currently hold it so the deferred Unlock never
	// fires on an already-unlocked mutex — a fatal, unrecoverable runtime error
	// that would take down the whole process. An early return on ctx.Done()
	// happens while unlocked, so the defer must be a no-op on that path.
	rl.mu.Lock()
	locked := true
	defer func() {
		if locked {
			rl.mu.Unlock()
		}
	}()

	// Reset counts every minute
	if time.Now().After(rl.resetTime) {
		rl.requestCounts = make(map[string]int)
		rl.resetTime = time.Now().Add(time.Minute)
	}

	// Check if we've exceeded the per-minute limit (max 10 requests per minute)
	if rl.requestCounts[key] >= 10 {
		waitTime := time.Until(rl.resetTime)
		rl.mu.Unlock()
		locked = false
		select {
		case <-time.After(waitTime):
		case <-ctx.Done():
			return ctx.Err()
		}
		rl.mu.Lock()
		locked = true
		rl.requestCounts = make(map[string]int)
		rl.resetTime = time.Now().Add(time.Minute)
	}

	// Wait for minimum interval between requests
	elapsed := time.Since(rl.lastRequest)
	if elapsed < rl.minInterval {
		waitTime := rl.minInterval - elapsed
		rl.mu.Unlock()
		locked = false
		select {
		case <-time.After(waitTime):
		case <-ctx.Done():
			return ctx.Err()
		}
		rl.mu.Lock()
		locked = true
	}

	rl.lastRequest = time.Now()
	rl.requestCounts[key]++
	return nil
}

// NewWebSearchTool creates a new web search tool
func NewWebSearchTool() *WebSearchTool {
	return &WebSearchTool{
		client: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		rateLimiter: newRateLimiter(2 * time.Second), // Minimum 2 seconds between requests
	}
}

func (t *WebSearchTool) Run(ctx context.Context, args map[string]interface{}) (string, error) {
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return "", errors.New("query parameter is required and must be a string")
	}

	maxResults := 10
	if mr, ok := args["max_results"].(float64); ok {
		maxResults = int(mr)
	}
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 20 {
		maxResults = 20
	}

	// Apply rate limiting
	if err := t.rateLimiter.wait(ctx, "search"); err != nil {
		return "", fmt.Errorf("rate limit wait cancelled: %w", err)
	}

	results, err := t.searchDuckDuckGo(ctx, query, maxResults)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	return t.formatSearchResults(results), nil
}

// searchDuckDuckGo performs a web search using DuckDuckGo's HTML endpoint
func (t *WebSearchTool) searchDuckDuckGo(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if maxResults <= 0 {
		maxResults = 10
	}

	formData := url.Values{}
	formData.Set("q", query)
	formData.Set("b", "")
	formData.Set("kl", "")

	req, err := http.NewRequestWithContext(ctx, "POST", "https://html.duckduckgo.com/html", strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", BrowserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Referer", "https://duckduckgo.com/")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute search: %w", err)
	}
	defer resp.Body.Close()

	// Accept both 200 (OK) and 202 (Accepted)
	// DuckDuckGo may still return 202 for rate limiting or bot detection
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("search failed with status code: %d (DuckDuckGo may be rate limiting requests)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return parseSearchResults(string(body), maxResults)
}

// parseSearchResults extracts search results from DuckDuckGo HTML response
func parseSearchResults(htmlContent string, maxResults int) ([]SearchResult, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML: %w", err)
	}

	var results []SearchResult
	var traverse func(*html.Node)

	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "result") {
			result := extractResult(n)
			if result != nil && result.Link != "" && !strings.Contains(result.Link, "y.js") {
				result.Position = len(results) + 1
				results = append(results, *result)
				if len(results) >= maxResults {
					return
				}
			}
		}
		for c := n.FirstChild; c != nil && len(results) < maxResults; c = c.NextSibling {
			traverse(c)
		}
	}

	traverse(doc)
	return results, nil
}

// hasClass checks if an HTML node has a specific class
func hasClass(n *html.Node, class string) bool {
	for _, attr := range n.Attr {
		if attr.Key == "class" {
			val := attr.Val
			for {
				// Skip leading whitespace
				i := 0
				for i < len(val) && (val[i] == ' ' || val[i] == '\t' || val[i] == '\n' || val[i] == '\r' || val[i] == '\f') {
					i++
				}
				val = val[i:]
				if len(val) == 0 {
					break
				}

				// Find end of the current class name
				j := 0
				for j < len(val) && val[j] != ' ' && val[j] != '\t' && val[j] != '\n' && val[j] != '\r' && val[j] != '\f' {
					j++
				}

				// Check if it matches the target class
				if val[:j] == class {
					return true
				}

				// Move to the next part of the string
				val = val[j:]
			}
		}
	}
	return false
}

// extractResult extracts a search result from a result div node
func extractResult(n *html.Node) *SearchResult {
	result := &SearchResult{}

	var traverse func(*html.Node)
	traverse = func(node *html.Node) {
		if node.Type == html.ElementNode {
			// Look for title link
			if node.Data == "a" && hasClass(node, "result__a") {
				result.Title = getTextContent(node)
				for _, attr := range node.Attr {
					if attr.Key == "href" {
						result.Link = cleanDuckDuckGoURL(attr.Val)
						break
					}
				}
			}
			// Look for snippet
			if node.Data == "a" && hasClass(node, "result__snippet") {
				result.Snippet = getTextContent(node)
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			traverse(c)
		}
	}

	traverse(n)
	return result
}

// getTextContent extracts all text content from a node and its children
func getTextContent(n *html.Node) string {
	var text strings.Builder
	var traverse func(*html.Node)

	traverse = func(node *html.Node) {
		if node.Type == html.TextNode {
			text.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			traverse(c)
		}
	}

	traverse(n)
	return strings.TrimSpace(text.String())
}

// cleanDuckDuckGoURL extracts the actual URL from DuckDuckGo's redirect URL
func cleanDuckDuckGoURL(rawURL string) string {
	if strings.HasPrefix(rawURL, "//duckduckgo.com/l/?uddg=") {
		// Extract the actual URL from the redirect
		if idx := strings.Index(rawURL, "uddg="); idx != -1 {
			encoded := rawURL[idx+5:]
			if ampIdx := strings.Index(encoded, "&"); ampIdx != -1 {
				encoded = encoded[:ampIdx]
			}
			decoded, err := url.QueryUnescape(encoded)
			if err == nil {
				return decoded
			}
		}
	}
	return rawURL
}

// formatSearchResults formats search results for LLM consumption
func (t *WebSearchTool) formatSearchResults(results []SearchResult) string {
	if len(results) == 0 {
		return "No results were found for your search query. This could be due to DuckDuckGo's bot detection or the query returned no matches. Please try rephrasing your search or try again in a few minutes."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d search results:\n\n", len(results))

	for _, result := range results {
		fmt.Fprintf(&sb, "%d. %s\n", result.Position, result.Title)
		fmt.Fprintf(&sb, "   URL: %s\n", result.Link)
		fmt.Fprintf(&sb, "   Summary: %s\n\n", result.Snippet)
	}

	return sb.String()
}
