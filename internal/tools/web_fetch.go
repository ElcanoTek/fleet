package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"golang.org/x/net/html"
)

const (
	// BrowserUserAgent is a realistic browser User-Agent for better compatibility
	BrowserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// MaxResponseSize limits the size of fetched content to 5MB
	MaxResponseSize = 5 * 1024 * 1024

	// DefaultTimeout for HTTP requests
	DefaultTimeout = 30 * time.Second
)

var multipleNewlinesRe = regexp.MustCompile(`\n{3,}`)

// cacheEntry represents a cached web fetch result
type cacheEntry struct {
	content   string
	timestamp time.Time
}

// fetchCache provides simple in-memory caching for web fetches
type fetchCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
	ttl     time.Duration
}

func newFetchCache() *fetchCache {
	return &fetchCache{
		entries: make(map[string]*cacheEntry),
		ttl:     5 * time.Minute,
	}
}

func (c *fetchCache) get(url string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.entries[c.hashURL(url)]
	if !exists {
		return "", false
	}

	// Check if cache entry is still valid
	if time.Since(entry.timestamp) > c.ttl {
		return "", false
	}

	return entry.content, true
}

func (c *fetchCache) set(url, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[c.hashURL(url)] = &cacheEntry{
		content:   content,
		timestamp: time.Now(),
	}

	// Simple cleanup: remove expired entries if cache is getting large
	if len(c.entries) > 100 {
		c.cleanup()
	}
}

func (c *fetchCache) cleanup() {
	now := time.Now()
	for key, entry := range c.entries {
		if now.Sub(entry.timestamp) > c.ttl {
			delete(c.entries, key)
		}
	}
}

func (c *fetchCache) hashURL(url string) string {
	hash := sha256.Sum256([]byte(url))
	return hex.EncodeToString(hash[:])
}

// webFetchTool fetches web pages and converts them to markdown
type webFetchTool struct {
	client      *http.Client
	cache       *fetchCache
	rateLimiter *rateLimiter
}

// WebFetchParams are the typed parameters for the web_fetch tool.
type WebFetchParams struct {
	URL string `json:"url" description:"The URL to fetch content from."`
}

// isPrivateIP reports whether ip is an address the network tools must
// refuse to connect to: private (RFC1918), loopback, link-local
// (incl. the 169.254.169.254 cloud-metadata endpoint), and the
// unspecified address.
func isPrivateIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// newSSRFGuardedDialer returns a dialer that refuses connections to
// private, loopback, link-local, and unspecified addresses. The Control
// hook runs AFTER DNS resolution, so a hostname that resolves (or
// DNS-rebinds) to an internal IP is blocked too. Shared by web_fetch and
// download_url so the network tools enforce one consistent SSRF policy
// (cloud metadata endpoints, internal services).
func newSSRFGuardedDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return errors.New("failed to parse IP")
			}
			if isPrivateIP(ip) {
				return errors.New("access to private IP denied for security reasons")
			}
			return nil
		},
	}
}

// NewWebFetchTool creates a fantasy.AgentTool for fetching web content.
// FetchURLForContext fetches url host-side for the `@url` composer context handle
// (#517), reusing the SAME SSRF-guarded dialer, 5 MiB cap, UTF-8 check, and
// HTML→markdown / JSON-pretty conversion as the web_fetch tool. The guarded
// dialer refuses private / loopback / link-local targets on EVERY dial (so a
// redirect to an internal address is blocked too); a non-200 or oversized
// response is an error. Returned text is cleaned for prompt inclusion. It is a
// standalone function (not the tool) so the chat server can expand a handle
// without constructing a tool, while the single SSRF/extraction implementation
// stays here (one source of truth).
func FetchURLForContext(ctx context.Context, url string) (string, error) {
	client := &http.Client{
		Timeout:   DefaultTimeout,
		Transport: &http.Transport{DialContext: newSSRFGuardedDialer().DialContext},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", BrowserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}
	content := string(body)
	if !utf8.ValidString(content) {
		return "", errors.New("response content is not valid UTF-8")
	}

	contentType := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(contentType, "text/html"):
		if markdown, convErr := convertHTMLToMarkdown(removeNoisyElements(content)); convErr == nil {
			content = cleanupMarkdown(markdown)
		}
	case strings.Contains(contentType, "application/json"), strings.Contains(contentType, "text/json"):
		if formatted, fmtErr := formatJSON(content); fmtErr == nil {
			content = formatted
		}
	}
	return content, nil
}

func NewWebFetchTool() fantasy.AgentTool {
	dialer := newSSRFGuardedDialer()

	t := &webFetchTool{
		client: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext:         dialer.DialContext,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		cache:       newFetchCache(),
		rateLimiter: newRateLimiter(1 * time.Second),
	}
	return fantasy.NewAgentTool("web_fetch",
		"Fetch content from a URL and convert it to markdown format. Useful for reading web pages, documentation, articles, and API responses.",
		func(ctx context.Context, params WebFetchParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := t.run(ctx, params.URL)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

// webFetchResult is the structured JSON response returned by the web_fetch tool.
type webFetchResult struct {
	URL             string          `json:"url"`
	Stdout          string          `json:"stdout"`
	StatusCode      int             `json:"status_code"`
	ContentType     string          `json:"content_type,omitempty"`
	Cached          bool            `json:"cached"`
	ExecutionTimeMs int64           `json:"execution_time_ms"`
	ContentBytes    int             `json:"content_bytes"`
	Error           string          `json:"error,omitempty"`
	TruncationInfo  *truncationInfo `json:"truncation_info,omitempty"`
}

// webFetchTruncateThreshold matches bash/python thresholds.
const webFetchTruncateThreshold = 32768 // ~8K tokens

func (t *webFetchTool) run(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", errors.New("url is required")
	}

	start := time.Now()

	// Check cache first
	if cached, found := t.cache.get(url); found {
		result := webFetchResult{
			URL:             url,
			Stdout:          cached,
			StatusCode:      200,
			Cached:          true,
			ExecutionTimeMs: time.Since(start).Milliseconds(),
			ContentBytes:    len(cached),
		}
		truncateWebFetchResult(&result)
		jsonBytes, err := json.Marshal(result)
		if err != nil {
			return cached, err
		}
		return string(jsonBytes), nil
	}

	// Apply rate limiting
	if err := t.rateLimiter.wait(ctx, "fetch"); err != nil {
		return "", fmt.Errorf("rate limit wait cancelled: %w", err)
	}

	content, statusCode, contentType, err := t.fetchURLAndConvertStructured(ctx, url)
	elapsed := time.Since(start)

	result := webFetchResult{
		URL:             url,
		ExecutionTimeMs: elapsed.Milliseconds(),
		StatusCode:      statusCode,
		ContentType:     contentType,
	}

	if err != nil {
		result.Error = err.Error()
		result.Stdout = ""
	} else {
		result.Stdout = content
		result.ContentBytes = len(content)
		// Cache the result
		t.cache.set(url, content)
	}

	truncateWebFetchResult(&result)

	jsonBytes, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		if err != nil {
			return "", err
		}
		return content, nil
	}
	return string(jsonBytes), nil
}

// truncateWebFetchResult handles large content by saving to temp file.
func truncateWebFetchResult(result *webFetchResult) {
	contentBytes := []byte(result.Stdout)
	if len(contentBytes) <= webFetchTruncateThreshold {
		return
	}
	truncated, path := truncateWithFile(contentBytes, "webfetch")
	result.TruncationInfo = &truncationInfo{
		StdoutTruncated: true,
		StdoutFullPath:  path,
		StdoutFullBytes: len(contentBytes),
	}
	result.Stdout = truncated
}

// fetchURLAndConvertStructured fetches a URL and returns content along with HTTP metadata.
func (t *webFetchTool) fetchURLAndConvertStructured(ctx context.Context, url string) (content string, statusCode int, contentType string, err error) {
	req, reqErr := http.NewRequestWithContext(ctx, "GET", url, nil)
	if reqErr != nil {
		return "", 0, "", fmt.Errorf("failed to create request: %w", reqErr)
	}

	// Use realistic browser headers for better compatibility
	req.Header.Set("User-Agent", BrowserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, doErr := t.client.Do(req)
	if doErr != nil {
		return "", 0, "", fmt.Errorf("failed to fetch URL: %w", doErr)
	}
	defer resp.Body.Close()

	statusCode = resp.StatusCode
	contentType = resp.Header.Get("Content-Type")

	if resp.StatusCode != http.StatusOK {
		// Read error body for context
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		errPreview := strings.TrimSpace(string(errBody))
		if len(errPreview) > 200 {
			errPreview = errPreview[:200] + "..."
		}
		return "", statusCode, contentType, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errPreview)
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	if readErr != nil {
		return "", statusCode, contentType, fmt.Errorf("failed to read response body: %w", readErr)
	}

	content = string(body)

	if !utf8.ValidString(content) {
		return "", statusCode, contentType, errors.New("response content is not valid UTF-8")
	}

	// Convert HTML to markdown for better AI processing
	if strings.Contains(contentType, "text/html") {
		cleanedHTML := removeNoisyElements(content)
		markdown, convErr := convertHTMLToMarkdown(cleanedHTML)
		if convErr != nil {
			return "", statusCode, contentType, fmt.Errorf("failed to convert HTML to markdown: %w", convErr)
		}
		content = cleanupMarkdown(markdown)
	} else if strings.Contains(contentType, "application/json") || strings.Contains(contentType, "text/json") {
		formatted, fmtErr := formatJSON(content)
		if fmtErr == nil {
			content = formatted
		}
	}

	return content, statusCode, contentType, nil
}

// removeNoisyElements removes script, style, nav, header, footer, and other
// noisy elements from HTML to improve content extraction
func removeNoisyElements(htmlContent string) string {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		// If parsing fails, return original content
		return htmlContent
	}

	// Elements to remove entirely
	noisyTags := map[string]bool{
		"script":   true,
		"style":    true,
		"nav":      true,
		"header":   true,
		"footer":   true,
		"aside":    true,
		"noscript": true,
		"iframe":   true,
		"svg":      true,
	}

	var removeNodes func(*html.Node)
	removeNodes = func(n *html.Node) {
		var toRemove []*html.Node

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && noisyTags[c.Data] {
				toRemove = append(toRemove, c)
			} else {
				removeNodes(c)
			}
		}

		for _, node := range toRemove {
			n.RemoveChild(node)
		}
	}

	removeNodes(doc)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return htmlContent
	}

	return buf.String()
}

// cleanupMarkdown removes excessive whitespace and blank lines from markdown
func cleanupMarkdown(content string) string {
	// Collapse multiple blank lines into at most two
	content = multipleNewlinesRe.ReplaceAllString(content, "\n\n")

	// Remove trailing whitespace from each line
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	content = strings.Join(lines, "\n")

	// Trim leading/trailing whitespace
	content = strings.TrimSpace(content)

	return content
}

// convertHTMLToMarkdown converts HTML content to markdown format
func convertHTMLToMarkdown(htmlContent string) (string, error) {
	converter := md.NewConverter("", true, nil)

	markdown, err := converter.ConvertString(htmlContent)
	if err != nil {
		return "", err
	}

	return markdown, nil
}

// formatJSON formats JSON content with proper indentation
func formatJSON(content string) (string, error) {
	var data any
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		return "", err
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return "", err
	}

	return buf.String(), nil
}
