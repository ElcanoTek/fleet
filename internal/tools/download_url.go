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
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/diskguard"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/truncate"
)

// downloadURLDialContext is the dial function download_url's HTTP client
// uses. Production keeps the SSRF guard (shared with web_fetch via
// newSSRFGuardedDialer) so this tool can't fetch cloud metadata
// (169.254.169.254) or internal services and persist the result to disk.
// Tests exercising the tool against local httptest servers (loopback)
// substitute a plain dialer.
var downloadURLDialContext = newSSRFGuardedDialer().DialContext

// downloadURLDescription is the prompt-facing description of the
// download_url tool: the one generic URL → workspace fetch. It is how agents
// land a fast.io signed URL, an email attachment or report link an email
// connector hands out, a public asset, or any other URL that just needs to be
// on disk. Client connector names stay out of it: they live in bundles.
const downloadURLDescription = "Download bytes from any HTTP(S) URL straight into your per-conversation workspace. " +
	"Use this after `mcp_fast_io_download action=file-url` to pull the returned opaque `fleet-download://...` handle's bytes onto disk; Fleet keeps the real signed URL server-side so its bearer token never enters your context. " +
	"Use it for email reports too: when an email connector hands you a short-lived download URL for an attachment or a report link, fetch it right away. " +
	"It also accepts ordinary HTTP(S) URLs for other one-call URL→scratch fetches (public CSVs, signed S3 links, generated reports, etc.).\n\n" +
	"BEHAVIOR: follows redirects (chain reported as `redirect_chain`), sends a realistic browser User-Agent so CDNs and signed-URL gateways don't reject the request, derives a filename from `Content-Disposition` then the URL path, suffixes a deterministic 8-char hash to avoid collisions in the workspace, and caps responses at 512 MB (a file over the cap is not saved at all). " +
	"When the link answers with an HTML interstitial (an email click-tracker's \"your download will start\" page, a meta refresh, or a page whose only link is the file) it follows it to the file, up to 3 times, and lists the hops in `html_chase`. " +
	"The result carries the saved file's `sha256`, and a `warning` when a link that names a data file (.csv, .xlsx, .zip, ...) returned an HTML page instead: usually a login page or an expired link.\n\n" +
	"REQUIRED: `url` (http, https, or a fleet-download handle returned by mcp_fast_io_download).\n" +
	"OPTIONAL: `output_dir` (defaults to the per-conversation workspace; a relative path is interpreted against it; an absolute path must be inside the chat-server's allowed dirs), `filename` (override the auto-detected name; the collision suffix is still appended), `timeout_seconds` (whole download; default 300, max 1800).\n\n" +
	"NOT FOR: HTML pages you want to read as markdown — use `web_fetch` for that. Private or internal addresses are refused."

// DownloadURLParams is the typed tool surface.
type DownloadURLParams struct {
	URL            string `json:"url" description:"HTTP/HTTPS URL, or an opaque fleet-download:// handle returned by mcp_fast_io_download. Required."`
	OutputDir      string `json:"output_dir,omitempty" description:"Directory to save the file in. Defaults to the per-conversation workspace (same dir bash/run_python land files in). A relative path is interpreted against the workspace; an absolute path must be inside the chat-server's allowed dirs."`
	Filename       string `json:"filename,omitempty" description:"Override the auto-detected filename. The collision-suffix is still appended so repeated downloads of the same URL don't overwrite each other."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" description:"Timeout in seconds for the whole download. Defaults to 300. Capped at 1800."`
}

// downloadURLMaxBytes caps one download. The archive's link cache keeps report
// files up to 450 MB (production-support ses-s3), so the cap sits above that.
// The body is never held whole: it streams into the sandbox in
// downloadURLChunkBytes pieces (FileOpStream), so memory stays at about one
// chunk however large the file. Variables so tests can shrink them.
var (
	downloadURLMaxBytes   int64 = 512 * 1024 * 1024
	downloadURLChunkBytes       = 16 * 1024 * 1024
)

const (
	// downloadURLHTMLScanBytes is how much of an HTML response is scanned for
	// an interstitial's target. Never a size limit: the page is saved whole.
	downloadURLHTMLScanBytes = 1 << 20
	// downloadURLMaxInterstitials bounds the interstitial pages followed.
	downloadURLMaxInterstitials = 3
)

const (
	downloadURLDefaultTimeout = 300 * time.Second
	downloadURLMaxTimeout     = 1800 * time.Second
	downloadURLDefaultName    = "download"

	downloadStatusSuccess = "success"
	downloadStatusError   = "error"
)

// downloadURLResult is the JSON payload returned to the agent.
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
	// HTMLChase lists the interstitial pages' targets followed to the file.
	HTMLChase []string `json:"html_chase,omitempty"`
	SHA256    string   `json:"sha256,omitempty"`
	// Deduplicated: the body matched an earlier download of the same URL in
	// that directory, so that file was kept and no copy was written.
	Deduplicated bool   `json:"deduplicated,omitempty"`
	Warning      string `json:"warning,omitempty"`
	Error        string `json:"error,omitempty"`
}

// NewDownloadURLTool returns the native download_url tool, bound to the
// per-turn sandbox. The HTTP fetch itself stays host-side by design — it is
// the ADR-0036 brokered-network class (SSRF guard, fleet-download:// handle
// resolution that keeps signed URLs out of the model's reach) — but the
// fetched bytes land on disk through the sandbox FileOp seam (#1083), so the
// tool performs no host file I/O. A nil sandbox fails closed.
func NewDownloadURLTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	return fantasy.NewAgentTool("download_url", downloadURLDescription,
		func(ctx context.Context, params DownloadURLParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			payload := runDownloadURL(ctx, sb, params)
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

// runDownloadURL is the testable core: validate → fetch → sandboxed write.
// It always returns a populated downloadURLResult — the caller decides
// how to surface success vs error.
func runDownloadURL(ctx context.Context, sb *sandbox.Sandbox, params DownloadURLParams) downloadURLResult {
	displayURL := strings.TrimSpace(params.URL)
	res := downloadURLResult{URL: displayURL}

	if sb == nil {
		res.Status = downloadStatusError
		res.Error = "download_url requires a sandbox; pool.Take returned nil or was bypassed"
		return res
	}
	if displayURL == "" {
		res.Status = downloadStatusError
		res.Error = "url is required"
		return res
	}
	raw, protected, err := resolveDownloadURLHandle(displayURL)
	if err != nil {
		res.Status = downloadStatusError
		res.Error = err.Error()
		return res
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("invalid url: only http/https URLs or fleet-download handles are supported (got %q)", displayURL)
		return res
	}

	dir, err := resolveDownloadDir(ctx, params.OutputDir)
	if err != nil {
		res.Status = downloadStatusError
		res.Error = err.Error()
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
	// 10 hops, the same as httpx.
	var chain []string
	// A per-call cookie jar: an interstitial that sets a session cookie
	// usually wants it back on the request for the file.
	jar, err := cookiejar.New(nil)
	if err != nil {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("cookie jar: %v", err)
		return res
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: timeout,
		// Same SSRF guard as web_fetch: without it this tool could fetch
		// cloud metadata (169.254.169.254) or internal services and
		// persist the result to disk.
		Transport: &http.Transport{
			DialContext: downloadURLDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			chain = append(chain, req.URL.String())
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}
	// A fresh Transport per call keeps pooled keep-alive connections (and
	// their read/write goroutines) alive until the REMOTE closes them; release
	// them with the call, as browserbase_live_view does.
	defer client.CloseIdleConnections()

	get := func(target string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("User-Agent", BrowserUserAgent)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "en-US,en;q=0.5")
		return client.Do(req)
	}

	resp, err := get(raw)
	if err != nil {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("fetch %s: %v", displayURL, transportErrForDisplay(err, protected))
		if len(chain) > 0 && !protected {
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
	if !protected {
		res.FinalURL = finalURL
	}
	res.HTTPStatus = resp.StatusCode
	res.ContentType = resp.Header.Get("Content-Type")

	switch {
	case protected:
		// Redirect targets can contain fresh bearer credentials too. The opaque
		// handle is all the model needs, so keep the entire resolved chain inside
		// the host process.
		chain = nil
	case len(chain) == 0:
		// No redirects fired; chain is just the initial → final URL.
		// Keep one entry when start == final (no actual redirect).
		if finalURL != raw {
			chain = []string{raw, finalURL}
		} else {
			chain = []string{raw}
		}
	default:
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
		preview := truncate.Clamp(strings.TrimSpace(string(errBody)), 200, "...")
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, preview)
		return res
	}

	resp, body, closeChased, err := followInterstitials(resp, get, protected, &res) //nolint:bodyclose // closeChased closes every body it opened; the first is closed by the defer above
	defer closeChased()
	if err != nil {
		res.Status = downloadStatusError
		res.Error = err.Error()
		return res
	}
	finalURL = resp.Request.URL.String()
	if !protected && len(res.HTMLChase) > 0 {
		// CheckRedirect kept appending the chased requests' redirects.
		res.RedirectChain = chain
	}
	if isHTMLResponse(resp) && namedADataFile(parsed.Path, resp.Request.URL.Path, res.HTMLChase) {
		res.Warning = "the server returned an HTML page, not the file the link names: often a login page or an expired link. Check the saved page before parsing it."
	}

	if resp.ContentLength > downloadURLMaxBytes {
		res.Status = downloadStatusError
		res.Error = downloadTooLarge()
		return res
	}

	pickedName := pickDownloadFilename(params.Filename, resp, finalURL)
	names := collisionSafeNames(pickedName, displayURL)
	validFirst, err := ValidatePath(filepath.Join(dir, names(0)))
	if err != nil {
		res.Status = downloadStatusError
		res.Error = fmt.Sprintf("output path validation failed: %v", err)
		return res
	}
	validDir := filepath.Dir(validFirst)
	room := downloadRoom(dir, resp.ContentLength)
	if err := room(0); err != nil {
		res.Status = downloadStatusError
		res.Error = err.Error()
		return res
	}
	// Stream the body into the sandbox through the FileOp seam — the write
	// inherits the container's runtime, limits, and confinement instead of
	// running as host os I/O (#1083) — a chunk at a time into a hidden partial
	// file, renamed into place only once the whole body arrived. A re-download
	// of the same URL whose bytes match an earlier copy reuses that copy, so
	// retries do not pile up duplicates in the workspace.
	choose := downloadDestination(ctx, sb, dir, validDir, names, pickedName)
	target, size, digest, err := sandboxStreamWrite(ctx, sb, dir, validDir, body, downloadURLMaxBytes, downloadURLChunkBytes, room, choose)
	if err != nil {
		res.Status = downloadStatusError
		if errors.Is(err, errStreamTooLarge) {
			res.Error = downloadTooLarge()
		} else {
			res.Error = fmt.Sprintf("save into %s: %v", validDir, err)
		}
		return res
	}
	validOut := target.Path
	res.Deduplicated = target.Reuse

	res.Status = downloadStatusSuccess
	res.Filename = filepath.Base(validOut)
	res.SavedTo = validOut
	res.SizeBytes = size
	res.SHA256 = digest
	return res
}

// followInterstitials follows an HTML interstitial (an email click-tracker's
// "your download will start" page, a meta refresh) to the file it stands in
// front of, at most downloadURLMaxInterstitials times, recording each hop in
// res. It returns the response to save, the body to read (an HTML page's
// scanned head is replayed in front of the rest), and a func closing every
// body it opened. A page that names no single file is saved as it is, and
// so is one whose target fails.
func followInterstitials(resp *http.Response, get func(string) (*http.Response, error), protected bool, res *downloadURLResult) (*http.Response, io.Reader, func(), error) {
	var opened []*http.Response
	closeAll := func() {
		for _, r := range opened {
			_ = r.Body.Close()
		}
	}
	var body io.Reader = resp.Body
	for hops := 0; !protected && isHTMLResponse(resp); hops++ {
		peek, err := io.ReadAll(io.LimitReader(resp.Body, downloadURLHTMLScanBytes))
		if err != nil {
			return resp, nil, closeAll, fmt.Errorf("read response body: %w", err)
		}
		body = io.MultiReader(bytes.NewReader(peek), resp.Body)
		target := ""
		if hops < downloadURLMaxInterstitials {
			target = interstitialTarget(peek, resp.Request.URL)
		}
		if target == "" {
			break
		}
		next := fetchInterstitialTarget(get, target)
		if next == nil {
			break
		}
		opened = append(opened, next)
		resp = next
		body = resp.Body
		res.HTMLChase = append(res.HTMLChase, target)
		res.FinalURL = resp.Request.URL.String()
		res.HTTPStatus = resp.StatusCode
		res.ContentType = resp.Header.Get("Content-Type")
	}
	return resp, body, closeAll, nil
}

// namedADataFile reports whether the requested URL, the final one, or any
// interstitial target followed on the way names a data file.
func namedADataFile(requested, final string, chased []string) bool {
	if looksLikeDataFile(requested) || looksLikeDataFile(final) {
		return true
	}
	for _, c := range chased {
		if u, err := url.Parse(c); err == nil && looksLikeDataFile(u.Path) {
			return true
		}
	}
	return false
}

// fetchInterstitialTarget fetches an interstitial's target, or returns nil
// when it fails: the interstitial page is then saved instead.
func fetchInterstitialTarget(get func(string) (*http.Response, error), target string) *http.Response {
	next, err := get(target)
	if err != nil {
		return nil
	}
	if next.StatusCode >= 400 {
		_ = next.Body.Close()
		return nil
	}
	return next
}

func downloadTooLarge() string {
	return fmt.Sprintf("response exceeded the %d-byte cap; nothing was saved.", downloadURLMaxBytes)
}

// resolveDownloadDir returns the absolute directory the download
// should land in, validating it stays inside the chat-server's allowed
// dirs. Empty or relative paths resolve against the per-conversation
// workspace; absolute paths are validated against [AllowedBaseDirs].
func resolveDownloadDir(ctx context.Context, requested string) (string, error) {
	trimmed := strings.TrimSpace(requested)

	// A forced working dir (scheduled worktree runs, delegated children) is
	// the root bash / run_python / view_file already resolve relative paths
	// against, and the root the sandbox FileOp seam confines writes to. It
	// has to anchor download_url the same way: before this branch a relative
	// output_dir in a scheduled run fell through to filepath.Abs against the
	// PROCESS cwd (/var/lib/fleet) — and the sandbox probe then refused the
	// very path fleet had just chosen ("path escapes the scheduled-run
	// worktree"). Observed as dozens of failed download_url calls per daily
	// refresh run for `output_dir: "sources"`, while an omitted output_dir
	// happened to work only because the shared workspace root WAS the forced
	// root for non-worktree tasks.
	if forced := ForcedWorkingDirFromContext(ctx); forced != "" {
		return resolveForcedDownloadDir(forced, trimmed)
	}

	// Empty or "." → per-conversation workspace.
	if trimmed == "" || trimmed == "." {
		if convID := ConversationIDFromContext(ctx); convID != "" {
			return filepath.Abs(WorkspaceDirForConversation(convID))
		}
		return filepath.Abs(WorkspaceDirForConversation(""))
	}

	if !filepath.IsAbs(trimmed) {
		// Reject any ".." component BEFORE joining (#564): filepath.Join
		// collapses ".." away, so a relative output_dir with enough ".."
		// segments would otherwise escape the workspace anchor (and the
		// process cwd on the no-convID branch) and become a host-side
		// arbitrary write. With ".." gone, the join below cannot leave
		// its anchor directory.
		if containsDotDotComponent(trimmed) {
			return "", &PathSecurityError{
				Path:    trimmed,
				Reason:  "output_dir must not contain '..' components",
				BaseDir: WorkspaceDirForConversation(ConversationIDFromContext(ctx)),
			}
		}
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

// resolveForcedDownloadDir anchors output_dir to a forced per-run root:
// empty or "." is the root itself, a relative path joins under it (".."
// components rejected before the join, same as the workspace branch), and an
// absolute path must already lie inside it. The error names the root so the
// model can correct the call instead of retrying the same absolute path.
func resolveForcedDownloadDir(forced, trimmed string) (string, error) {
	root, err := filepath.Abs(forced)
	if err != nil {
		return "", fmt.Errorf("resolve forced working dir %s: %w", forced, err)
	}
	root = filepath.Clean(root)
	if trimmed == "" || trimmed == "." {
		return root, nil
	}
	if !filepath.IsAbs(trimmed) {
		if containsDotDotComponent(trimmed) {
			return "", &PathSecurityError{
				Path:    trimmed,
				Reason:  "output_dir must not contain '..' components",
				BaseDir: root,
			}
		}
		return filepath.Clean(filepath.Join(root, trimmed)), nil
	}
	abs := filepath.Clean(trimmed)
	if !isSubPath(root, abs) {
		return "", &PathSecurityError{
			Path:    trimmed,
			Reason:  "output_dir is outside the scheduled-run worktree; pass a relative output_dir (it lands under the worktree) or an absolute path inside it",
			BaseDir: root,
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

// collisionSafeNames mirrors the retired Python email server's filename
// strategy: an 8-char hash of the source URL, so repeated downloads of the
// same URL land on predictable names (and an identical one can be reused),
// while different URLs with the same filename never collide. names(0) is
// `<stem>__<hash><ext>`, names(i) `<stem>__<hash>_<i><ext>`.
func collisionSafeNames(filename, sourceURL string) func(i int) string {
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	if stem == "" {
		stem = downloadURLDefaultName
	}
	sum := sha256.Sum256([]byte(sourceURL))
	token := hex.EncodeToString(sum[:])[:8]
	return func(i int) string {
		if i == 0 {
			return fmt.Sprintf("%s__%s%s", stem, token, ext)
		}
		return fmt.Sprintf("%s__%s_%d%s", stem, token, i, ext)
	}
}

// downloadDestination picks where a streamed body lands from its SHA-256:
// the first free collision-safe name, or an earlier download of the same URL
// whose bytes are identical (reused, so retries do not pile up copies).
func downloadDestination(ctx context.Context, sb *sandbox.Sandbox, dir, validDir string, names func(int) string, pickedName string) func(string) (streamTarget, error) {
	return func(sum string) (streamTarget, error) {
		for i := 0; i < 1000; i++ {
			candidate := filepath.Join(validDir, names(i))
			exists, existing, err := sandboxFileSHA256(ctx, sb, filepath.Join(dir, names(i)), candidate)
			if err != nil {
				return streamTarget{}, fmt.Errorf("probe %s: %w", candidate, err)
			}
			if !exists {
				return streamTarget{Path: candidate}, nil
			}
			if existing == sum {
				return streamTarget{Path: candidate, Reuse: true}, nil
			}
		}
		return streamTarget{}, fmt.Errorf("1000 different copies of %s already exist in %s", pickedName, validDir)
	}
}

// downloadRoom returns the disk-headroom check run before each chunk: a
// download never takes the filesystem holding dir below the reserve
// (downloadReserve), so a run of large downloads cannot fill the box. A body
// that declared its length is checked whole up front. The check fails open:
// a disk that cannot be measured does not block downloads.
func downloadRoom(dir string, declared int64) func(next int64) error {
	return func(next int64) error {
		// The directory may not exist yet: measure its nearest existing
		// ancestor, which is on the same filesystem.
		at := dir
		for {
			if _, err := os.Stat(at); err == nil || filepath.Dir(at) == at {
				break
			}
			at = filepath.Dir(at)
		}
		total, avail, err := downloadDiskUsage(at)
		if err != nil || total == 0 {
			return nil //nolint:nilerr // fails open, as diskguard does: an unmeasurable disk never blocks a download
		}
		need := next
		if next == 0 && declared > 0 {
			need = declared
		}
		need = max(need, 0)
		reserve := downloadReserve(total)
		if avail < reserve || avail-reserve < uint64(need) {
			return fmt.Errorf("not enough free disk space: %s free, and downloads keep at least %s free on this filesystem; free up space (Settings → Admin → Server → storage) and retry",
				humanBytes(avail), humanBytes(reserve))
		}
		return nil
	}
}

// downloadDiskUsage measures a filesystem; a variable so tests can stub it.
var downloadDiskUsage = diskguard.Usage

// downloadReserve is the free space a download must leave: 5% of the
// filesystem (the default FLEET_DISK_MIN_FREE_PERCENT floor, below which the
// box sheds scheduled work), and never less than 2 GiB.
func downloadReserve(total uint64) uint64 {
	return max(total/20, 2<<30)
}

func humanBytes(n uint64) string {
	const gib = 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MiB", n>>20)
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

// transportErrForDisplay renders a client.Do failure for the tool result. A
// *url.Error prints the FULL request URL — and for a protected
// fleet-download:// handle that is the vaulted bearer-carrying URL the handle
// exists to keep out of the model context (download_url_handle.go). The
// success path already suppresses FinalURL/RedirectChain when protected; the
// failure path used to hand the same URL straight back through the error
// string on any dial/TLS/timeout/redirect failure, including the SSRF guard's
// own refusal. When protected, report only the operation and the inner cause.
func transportErrForDisplay(err error, protected bool) string {
	if !protected {
		return err.Error()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		op := ue.Op
		if op == "" {
			op = "request"
		}
		if ue.Err != nil {
			return fmt.Sprintf("%s (protected URL): %v", op, ue.Err)
		}
		return op + " (protected URL) failed"
	}
	return err.Error()
}
