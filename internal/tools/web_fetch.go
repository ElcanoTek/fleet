package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"golang.org/x/net/html"

	"github.com/ElcanoTek/fleet/internal/netguard"
	"github.com/ElcanoTek/fleet/internal/truncate"
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

// webFetchTool fetches web pages and converts them to markdown.
//
// There is deliberately no fetch cache here. One used to sit on this struct,
// but NewWebFetchTool is called per turn, so the cache never outlived the turn
// that filled it, and it never evicted on read — the model saw a "cached" flag
// that was true only for a repeat fetch of the same URL inside one turn, and a
// stale body if the page changed in that window. A cache worth having would be
// process-wide, bounded and keyed per conversation; until one is needed, every
// fetch is live and the result says so by having no cached flag at all.
type webFetchTool struct {
	client      *http.Client
	rateLimiter *rateLimiter
}

// WebFetchParams are the typed parameters for the web_fetch tool.
type WebFetchParams struct {
	URL string `json:"url" description:"The URL to fetch content from."`
}

// isPrivateIP reports whether ip is an address the network tools must refuse
// to connect to. It delegates to the shared SSRF classifier so web_fetch and
// download_url block exactly the same ranges as every other outbound path (see
// internal/netguard) — one source of truth for this security invariant.
func isPrivateIP(ip net.IP) bool {
	return netguard.IsBlockedIP(ip)
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
	// Per-call Transport: release its pooled connections with the call rather
	// than leaving keep-alive goroutines parked until the remote hangs up.
	defer client.CloseIdleConnections()
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
	URL             string `json:"url"`
	Stdout          string `json:"stdout"`
	StatusCode      int    `json:"status_code"`
	ContentType     string `json:"content_type,omitempty"`
	ExecutionTimeMs int64  `json:"execution_time_ms"`
	ContentBytes    int    `json:"content_bytes"`
	Error           string `json:"error,omitempty"`
}

func (t *webFetchTool) run(ctx context.Context, url string) (string, error) {
	if url == "" {
		return "", errors.New("url is required")
	}

	start := time.Now()

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
	}

	jsonBytes, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		if err != nil {
			return "", err
		}
		return content, nil
	}
	return string(jsonBytes), nil
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
		errPreview := truncate.Clamp(strings.TrimSpace(string(errBody)), 200, "...")
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
