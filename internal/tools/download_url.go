package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"
)

// downloadURLDescription is the prompt-facing description of the
// download_url tool. The wording is deliberately namespace-neutral
// ("generic URL → workspace") so the agent reaches for this instead of
// the email-namespaced `download_link_attachment` when the source is a
// fast.io signed URL, a public asset, or any other arbitrary URL that
// just needs to land on disk. The email MCP tool keeps working for
// back-compat — it just shouldn't be the obvious match for non-email
// flows anymore.
const downloadURLDescription = "Download bytes from any HTTP(S) URL straight into your per-conversation workspace. " +
	"Use this after `mcp_fast_io_download action=file-url` to pull the signed URL's bytes onto disk, or for any other one-call URL→scratch fetch (public CSVs, signed S3 links, generated reports, etc.). " +
	"This is the namespace-neutral alternative to `mcp_email_download_link_attachment` — same single-call shape (URL in, file path out), but not tied to the email server.\n\n" +
	"BEHAVIOR: follows redirects (chain reported as `redirect_chain`), sends a realistic browser User-Agent so CDNs and signed-URL gateways don't reject the request, derives a filename from `Content-Disposition` then the URL path, suffixes a deterministic 8-char hash to avoid collisions in the workspace, and caps responses at 100 MB.\n\n" +
	"REQUIRED: `url` (http or https).\n" +
	"OPTIONAL: `output_dir` (defaults to the per-conversation workspace; a relative path is interpreted against it; an absolute path must be inside the chat-server's allowed dirs), `filename` (override the auto-detected name; the collision suffix is still appended), `timeout_seconds` (default 120, max 600).\n\n" +
	"NOT FOR: HTML pages you want to read as markdown — use `web_fetch` for that. Email click-trackers that need HTML-chase to reach the real download — use the email MCP's `download_link_attachment` instead (it has the SendGrid/Mailchimp interstitial bypass)."

// DownloadURLParams is the typed tool surface.
type DownloadURLParams struct {
	URL            string `json:"url" description:"HTTP or HTTPS URL to download. Required."`
	OutputDir      string `json:"output_dir,omitempty" description:"Directory to save the file in. Defaults to the per-conversation workspace (same dir bash/run_python land files in). A relative path is interpreted against the workspace; an absolute path must be inside the chat-server's allowed dirs."`
	Filename       string `json:"filename,omitempty" description:"Override the auto-detected filename. The collision-suffix is still appended so repeated downloads of the same URL don't overwrite each other."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" description:"Per-request timeout in seconds. Defaults to 120. Capped at 600."`
}

const (
	downloadURLDefaultTimeout = 120 * time.Second
	downloadURLMaxTimeout     = 600 * time.Second
	downloadURLMaxBytes       = 100 * 1024 * 1024
	downloadURLDefaultName    = "download"

	downloadStatusSuccess = "success"
	downloadStatusError   = "error"
)

// downloadURLResult is the JSON payload returned to the agent. The
// field set matches `mcp_email_download_link_attachment` so anything
// downstream that parses one shape can parse the other.
type downloadURLResult struct {
	Status        string   `json:"status"`
	URL           string   `json:"url"`
	FinalURL      string   `json:"final_url,omitempty"`
	Filename      string   `json:"filename,omitempty"`
	SavedTo       string   `json:"saved_to,omitempty"`
	SizeBytes     int64    `json:"size_bytes,omitempty"`
	ContentType   string   `json:"content_type,omitempty"`
	HTTPStatus    int      `json:"http_status,omitempty"`
	RedirectChain []string `json:"redirect_chain,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// NewDownloadURLTool returns the native download_url tool. It carries
// no external dependencies (no MCP client, no sandbox) — purely a
// Go-side HTTP fetcher that writes into the per-conversation workspace.
func NewDownloadURLTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("download_url", downloadURLDescription,
		func(ctx context.Context, params DownloadURLParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			payload := runDownloadURL(ctx, params)
			body, err := json.Marshal(payload)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if payload.Status != downloadStatusSuccess {
				return fantasy.NewTextErrorResponse(string(body)), nil
			}
			return fantasy.NewTextResponse(string(body)), nil
		})
}

// runDownloadURL is the testable core: validate → fetch → write.
// It always returns a populated downloadURLResult — the caller decides
// how to surface success vs error.
func runDownloadURL(ctx context.Context, params DownloadURLParams) downloadURLResult {
	res := downloadURLResult{URL: params.URL}

	raw := strings.TrimSpace(params.URL)
	if raw == "" {
		res.Status = downloadStatusError
		res.Error = "url is required"
		return res
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("invalid url: only http/https schemes are supported (got %q)", raw)
		return res
	}

	dir, err := resolveDownloadDir(ctx, params.OutputDir)
	if err != nil {
		res.Status = downloadStatusError
		res.Error = err.Error()
		return res
	}
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil { //nolint:gosec // dir already validated against allowed roots
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("create output_dir: %v", mkErr)
		return res
	}

	timeout := downloadURLDefaultTimeout
	if params.TimeoutSeconds > 0 {
		timeout = time.Duration(params.TimeoutSeconds) * time.Second
		if timeout > downloadURLMaxTimeout {
			timeout = downloadURLMaxTimeout
		}
	}

	// CheckRedirect runs before each hop; we use it to capture the chain
	// of URLs the redirector walks through. The default policy stops at
	// 10 hops, which matches what httpx does in the email tool.
	var chain []string
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			chain = append(chain, req.URL.String())
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("build request: %v", err)
		return res
	}
	req.Header.Set("User-Agent", BrowserUserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("fetch %s: %v", raw, err)
		if len(chain) > 0 {
			res.RedirectChain = chain
		}
		return res
	}
	defer resp.Body.Close()

	// Build the redirect chain: start = initial URL, then the URL each
	// hop landed on. CheckRedirect appends *upcoming* hops, so we add
	// the initial URL up front and rely on the captured intermediate
	// hops plus the final URL from resp.Request.
	finalURL := resp.Request.URL.String()
	res.FinalURL = finalURL
	res.HTTPStatus = resp.StatusCode
	res.ContentType = resp.Header.Get("Content-Type")

	if len(chain) == 0 {
		// No redirects fired; chain is just the initial → final URL.
		// Keep one entry when start == final (no actual redirect).
		if finalURL != raw {
			chain = []string{raw, finalURL}
		} else {
			chain = []string{raw}
		}
	} else {
		// CheckRedirect captured the chain mid-flight; prepend the
		// initial URL and append the final hop's resolved URL.
		// (chain currently holds the redirect *targets*, not the
		// initial URL.)
		out := append([]string{raw}, chain...)
		if last := out[len(out)-1]; last != finalURL {
			out = append(out, finalURL)
		}
		chain = out
	}
	res.RedirectChain = chain

	if resp.StatusCode >= 400 {
		// Preserve a snippet of the error body so the model can react
		// to e.g. "Token expired" without an extra fetch.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		preview := strings.TrimSpace(string(errBody))
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, preview)
		return res
	}

	pickedName := pickDownloadFilename(params.Filename, resp, finalURL)
	outPath := buildCollisionSafePath(dir, pickedName, raw)

	f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // outPath is built from resolveDownloadDir() + a sanitized basename; both are pre-validated
	if err != nil {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("open %s: %v", outPath, err)
		return res
	}
	written, err := io.Copy(f, io.LimitReader(resp.Body, downloadURLMaxBytes+1))
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(outPath)
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("write %s: %v", outPath, err)
		return res
	}
	if closeErr != nil {
		_ = os.Remove(outPath)
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("close %s: %v", outPath, closeErr)
		return res
	}
	if written > downloadURLMaxBytes {
		_ = os.Remove(outPath)
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("response exceeded %d-byte cap; refusing to save partial file. Use bash + curl or run_python with a streaming download if the file is genuinely this large.", downloadURLMaxBytes)
		return res
	}

	res.Status = downloadStatusSuccess
	res.Filename = filepath.Base(outPath)
	res.SavedTo = outPath
	res.SizeBytes = written
	return res
}

// resolveDownloadDir returns the absolute directory the download
// should land in, validating it stays inside the chat-server's allowed
// dirs. Empty or relative paths resolve against the per-conversation
// workspace; absolute paths are validated against [AllowedBaseDirs].
func resolveDownloadDir(ctx context.Context, requested string) (string, error) {
	trimmed := strings.TrimSpace(requested)

	// Empty or "." → per-conversation workspace (matches the rewrite
	// behavior the email MCP relies on).
	if trimmed == "" || trimmed == "." {
		if convID := ConversationIDFromContext(ctx); convID != "" {
			return filepath.Abs(WorkspaceDirForConversation(convID))
		}
		return filepath.Abs(WorkspaceDirForConversation(""))
	}

	if !filepath.IsAbs(trimmed) {
		// Relative path: anchor to the per-conversation workspace so
		// `output_dir="subdir"` lands under workspace/<conv>/subdir
		// rather than the chat-server's cwd.
		if convID := ConversationIDFromContext(ctx); convID != "" {
			joined := filepath.Join(WorkspaceDirForConversation(convID), trimmed)
			return filepath.Abs(joined)
		}
		return filepath.Abs(trimmed)
	}

	// Absolute path: must be inside an allowed base dir.
	allowed, err := AllowedBaseDirs()
	if err != nil {
		return "", fmt.Errorf("allowed dirs: %w", err)
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", trimmed, err)
	}
	abs = filepath.Clean(abs)
	if !isSubPathAny(allowed, abs) {
		return "", &PathSecurityError{
			Path:    trimmed,
			Reason:  "output_dir is outside allowed directories",
			BaseDir: strings.Join(allowed, ":"),
		}
	}
	return abs, nil
}

var contentDispositionFilenameRe = regexp.MustCompile(`(?i)filename\*?=(?:UTF-8'')?["']?([^"';\n]+)`)

// pickDownloadFilename returns the bare filename (no path) the response
// should be saved under. Order:
//  1. caller-supplied override (sanitized)
//  2. Content-Disposition filename
//  3. URL path basename (when it has an extension)
//  4. extension inferred from Content-Type
//  5. fallback `download`
func pickDownloadFilename(override string, resp *http.Response, finalURL string) string {
	if name := sanitizeDownloadFilename(override); name != "" {
		return name
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if m := contentDispositionFilenameRe.FindStringSubmatch(cd); len(m) > 1 {
			candidate := strings.TrimSpace(m[1])
			if decoded, err := url.QueryUnescape(candidate); err == nil {
				candidate = decoded
			}
			if name := sanitizeDownloadFilename(candidate); name != "" {
				return name
			}
		}
	}
	if parsed, err := url.Parse(finalURL); err == nil {
		pathName := filepath.Base(parsed.Path)
		if pathName != "" && pathName != "/" && pathName != "." {
			if decoded, derr := url.QueryUnescape(pathName); derr == nil {
				pathName = decoded
			}
			if strings.Contains(pathName, ".") {
				if name := sanitizeDownloadFilename(pathName); name != "" {
					return name
				}
			}
		}
	}
	// Last resort: synthesize a name from the content type. We pick a
	// few common types we actually see in chat (xlsx, csv, pdf, json,
	// zip); anything else falls back to `.bin`.
	ext := extensionFromContentType(resp.Header.Get("Content-Type"))
	return downloadURLDefaultName + ext
}

// sanitizeDownloadFilename strips path separators, control characters,
// and trims to a sane length. Returns "" when the input is unusable so
// the caller can fall through to the next filename source.
func sanitizeDownloadFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Drop any path component the source might have included
	// (`../etc/passwd`, `subdir/foo.csv`, etc.).
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimLeft(s, ".")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimSpace(strings.Trim(b.String(), "._- "))
	if out == "" {
		return ""
	}
	if len(out) > 120 {
		// Preserve the extension when truncating.
		ext := filepath.Ext(out)
		stem := strings.TrimSuffix(out, ext)
		if len(ext) > 16 {
			// Pathological "extension" — drop it rather than keeping
			// half the filename in the extension slot.
			ext = ""
			stem = out
		}
		keep := 120 - len(ext)
		if keep < 1 {
			keep = 1
		}
		if len(stem) > keep {
			stem = stem[:keep]
		}
		out = stem + ext
	}
	return out
}

// buildCollisionSafePath mirrors the email tool's filename strategy:
// append an 8-char hash of the source URL so repeated downloads of the
// same URL into the same dir produce a deterministic, predictable
// path. If a file at that path already exists (e.g. the same URL was
// downloaded twice in one conversation), tack on `_1`, `_2`, … until
// a free slot is found.
func buildCollisionSafePath(dir, filename, sourceURL string) string {
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	if stem == "" {
		stem = downloadURLDefaultName
	}
	sum := sha256.Sum256([]byte(sourceURL))
	token := hex.EncodeToString(sum[:])[:8]
	candidate := filepath.Join(dir, fmt.Sprintf("%s__%s%s", stem, token, ext))
	for i := 1; i < 1000; i++ {
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s__%s_%d%s", stem, token, i, ext))
	}
	// Astronomically unlikely; surface a deterministic fallback so the
	// caller still gets *some* path back rather than an infinite loop.
	return filepath.Join(dir, fmt.Sprintf("%s__%s_overflow%s", stem, token, ext))
}

// extensionFromContentType maps a few common MIME types to extensions
// we actually see in chat. Falls back to `.bin` for unknown types so
// the file at least gets a real (non-empty) extension.
//
//nolint:goconst // switch-case lookup table — extracting MIME literals into named consts harms readability more than it helps.
func extensionFromContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/msword":
		return ".doc"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/pdf":
		return ".pdf"
	case "application/json":
		return ".json"
	case "application/zip":
		return ".zip"
	case "text/csv", "application/csv":
		return ".csv"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	}
	if exts, err := mime.ExtensionsByType(ct); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}
