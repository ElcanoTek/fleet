package playwrightrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/netguard"
)

const (
	defaultRunTimeout = 45 * time.Second
	maxRunTimeout     = 90 * time.Second
	maxOutputBytes    = 2 * 1024 * 1024 // 2 MiB stdout cap from runner
)

var errBlockedAddress = errors.New("playwright browser: connection to a private, loopback, or link-local address is not allowed")

// DialContext is the dialer used by the SSRF-guarded proxy that fronts every
// Chromium network request. Production uses netguard.IsBlockedIP; tests
// exercising loopback httptest servers may override it.
var DialContext = guardedDialContext(netguard.IsBlockedIP)

func guardedDialContext(isBlocked func(net.IP) bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split host:port: %w", err)
		}
		if ip := net.ParseIP(host); ip != nil {
			if isBlocked(ip) {
				return nil, errBlockedAddress
			}
			return dialer.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		var lastErr error
		for _, ipa := range ips {
			if isBlocked(ipa.IP) {
				lastErr = errBlockedAddress
				continue
			}
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if derr != nil {
				lastErr = derr
				continue
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = errBlockedAddress
		}
		return nil, lastErr
	}
}

// Step is one declarative browser action in a Playwright automation run.
type Step struct {
	// Action is one of: "goto", "fill", "click", "wait_for", "press",
	// "select", "extract", "eval_js", "sleep".
	Action     string `json:"action"`
	Selector   string `json:"selector,omitempty"`
	Value      string `json:"value,omitempty"`
	Mode       string `json:"mode,omitempty"`       // for extract: "text" (default), "markdown", "html", "table_json", "attribute"
	Attribute  string `json:"attribute,omitempty"`  // for extract when mode == "attribute"
	Expression string `json:"expression,omitempty"` // for eval_js
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
	Optional   bool   `json:"optional,omitempty"`
}

// Credentials holds host-side authentication secrets injected into a browser run.
// They are passed to the runner over stdin and never appear in argv; any wire
// occurrences are scrubbed from the returned output and error text.
type Credentials struct {
	Username     string            `json:"username,omitempty"`
	Password     string            `json:"password,omitempty"`
	Secret       string            `json:"secret,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	StorageState map[string]any    `json:"storage_state,omitempty"`
}

// RunRequest configures one headless Chromium Playwright session.
type RunRequest struct {
	URL            string         `json:"url"`
	Steps          []Step         `json:"steps,omitempty"`
	Credentials    Credentials    `json:"credentials,omitempty"`
	Args           map[string]any `json:"args,omitempty"`
	CaptureNetwork bool           `json:"capture_network,omitempty"`
	InspectDOM     bool           `json:"inspect_dom,omitempty"`
	ExportState    bool           `json:"export_state,omitempty"`
	Timeout        time.Duration  `json:"-"`
}

// DOMElement summarizes an interactive element or table discovered on the page.
type DOMElement struct {
	Tag         string `json:"tag"`
	Type        string `json:"type,omitempty"`
	Name        string `json:"name,omitempty"`
	ID          string `json:"id,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Text        string `json:"text,omitempty"`
	Selector    string `json:"selector"`
}

// NetworkCall summarizes an XHR/fetch/JSON request captured during the session.
type NetworkCall struct {
	Method          string `json:"method"`
	URL             string `json:"url"`
	Status          int    `json:"status"`
	ContentType     string `json:"content_type,omitempty"`
	ResponsePreview string `json:"response_preview,omitempty"`
}

// RunResult is the structured output of a headless Chromium Playwright session.
type RunResult struct {
	FinalURL     string         `json:"final_url"`
	Title        string         `json:"title"`
	Status       int            `json:"status,omitempty"`
	Content      string         `json:"content"`
	Extracted    []any          `json:"extracted,omitempty"`
	DOMElements  []DOMElement   `json:"dom_elements,omitempty"`
	NetworkCalls []NetworkCall  `json:"network_calls,omitempty"`
	StorageState map[string]any `json:"storage_state,omitempty"`
}

// ValidateStep validates a single declarative Playwright step.
func ValidateStep(st Step) error {
	switch strings.ToLower(strings.TrimSpace(st.Action)) {
	case "goto":
		if strings.TrimSpace(st.Value) == "" && strings.TrimSpace(st.Selector) == "" {
			return errors.New("goto step requires value (the target URL)")
		}
	case "fill":
		if strings.TrimSpace(st.Selector) == "" {
			return errors.New("fill step requires selector")
		}
	case "click":
		if strings.TrimSpace(st.Selector) == "" {
			return errors.New("click step requires selector")
		}
	case "wait_for":
		if strings.TrimSpace(st.Selector) == "" && strings.TrimSpace(st.Value) == "" && st.TimeoutMs <= 0 {
			return errors.New("wait_for step requires selector, value, or timeout_ms")
		}
	case "press":
		if strings.TrimSpace(st.Selector) == "" || strings.TrimSpace(st.Value) == "" {
			return errors.New("press step requires selector and value (key name, e.g. Enter)")
		}
	case "select":
		if strings.TrimSpace(st.Selector) == "" {
			return errors.New("select step requires selector")
		}
	case "extract":
		if strings.TrimSpace(st.Selector) == "" {
			return errors.New("extract step requires selector")
		}
	case "eval_js":
		if strings.TrimSpace(st.Expression) == "" && strings.TrimSpace(st.Value) == "" {
			return errors.New("eval_js step requires expression")
		}
	case "sleep":
		// valid; uses timeout_ms
	default:
		return fmt.Errorf("unsupported playwright step action %q (expected goto, fill, click, wait_for, press, select, extract, eval_js, or sleep)", st.Action)
	}
	return nil
}

// ValidateTargetURL checks that rawURL is a valid http(s) URL before launching Chromium.
func ValidateTargetURL(rawURL string, allowInsecureHTTP bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return "", errors.New("a valid http(s) URL is required")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && (scheme != "http" || !allowInsecureHTTP) {
		if scheme == "http" {
			return "", errors.New("URL must use https://")
		}
		return "", fmt.Errorf("unsupported URL scheme %q (use https://)", scheme)
	}
	if u.User != nil {
		return "", errors.New("URL must not embed userinfo credentials; pass credentials via the secret fields")
	}
	return u.String(), nil
}

// Run executes req inside a headless Chromium browser managed by Playwright,
// routing all browser traffic through an ephemeral host-side netguard SSRF proxy.
func Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	if timeout > maxRunTimeout {
		timeout = maxRunTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	targetURL := substituteArgs(strings.TrimSpace(req.URL), req.Args)
	if targetURL == "" {
		return nil, errors.New("playwright runner: url is required")
	}
	u, err := url.Parse(targetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("playwright runner: invalid http(s) URL %q", targetURL)
	}

	steps := make([]Step, 0, len(req.Steps))
	for _, st := range req.Steps {
		st.Action = strings.ToLower(strings.TrimSpace(st.Action))
		if err := ValidateStep(st); err != nil {
			return nil, err
		}
		st.Selector = substituteArgs(st.Selector, req.Args)
		st.Value = substituteArgs(st.Value, req.Args)
		st.Expression = substituteArgs(st.Expression, req.Args)
		steps = append(steps, st)
	}

	proxyAddr, closeProxy, err := startSSRFGuardProxy(runCtx, DialContext)
	if err != nil {
		return nil, fmt.Errorf("start SSRF guard proxy: %w", err)
	}
	defer closeProxy()

	payload := runnerInput{
		URL:            targetURL,
		Steps:          steps,
		Credentials:    req.Credentials,
		CaptureNetwork: req.CaptureNetwork,
		InspectDOM:     req.InspectDOM,
		ExportState:    req.ExportState,
		ProxyServer:    "http://" + proxyAddr,
		TimeoutMs:      int(timeout.Milliseconds()),
	}
	stdinBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode runner input: %w", err)
	}

	rawOut, runErr := executeRunnerProcess(runCtx, stdinBytes)
	var parsed runnerResponse
	if len(rawOut) > 0 {
		if jerr := json.Unmarshal(rawOut, &parsed); jerr == nil {
			if !parsed.OK {
				msg := maskSecrets(parsed.Error, req.Credentials)
				if msg == "" {
					msg = "playwright browser run failed"
				}
				return nil, errors.New(msg)
			}
			res := &parsed.Result
			scrubResult(res, req.Credentials)
			return res, nil
		}
	}
	if runErr != nil {
		return nil, fmt.Errorf("playwright browser failed: %s", maskSecrets(runErr.Error(), req.Credentials))
	}
	return nil, errors.New("playwright browser returned no output")
}

type runnerInput struct {
	URL            string      `json:"url"`
	Steps          []Step      `json:"steps"`
	Credentials    Credentials `json:"credentials"`
	CaptureNetwork bool        `json:"capture_network"`
	InspectDOM     bool        `json:"inspect_dom"`
	ExportState    bool        `json:"export_state"`
	ProxyServer    string      `json:"proxy_server"`
	TimeoutMs      int         `json:"timeout_ms"`
}

type runnerResponse struct {
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
	Result RunResult `json:"result"`
}

func executeRunnerProcess(ctx context.Context, stdinBytes []byte) ([]byte, error) {
	env := buildRunnerEnv()
	if pythonBin, err := exec.LookPath("python3"); err == nil {
		out, perr := runSubprocess(ctx, pythonBin, []string{"-c", embeddedPythonRunner}, env, stdinBytes)
		if len(out) > 0 && !bytes.Contains(out, []byte("playwright python package is not available")) {
			return out, perr
		}
	}
	if nodeBin, err := exec.LookPath("node"); err == nil {
		out, nerr := runSubprocess(ctx, nodeBin, []string{"-e", embeddedNodeRunner}, env, stdinBytes)
		if len(out) > 0 {
			return out, nerr
		}
		if nerr != nil {
			return nil, nerr
		}
	}
	return nil, errors.New("neither python3 playwright nor node playwright is available on this host")
}

func runSubprocess(ctx context.Context, bin string, args []string, env []string, stdinBytes []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is resolved via LookPath and args are compile-time embedded scripts.
	cmd.Stdin = bytes.NewReader(stdinBytes)
	cmd.Env = env
	var stdout, stderr cappedBuffer
	stdout.cap = maxOutputBytes
	stderr.cap = 64 * 1024
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil && strings.TrimSpace(stderr.String()) != "" {
		return stdout.Bytes(), errors.New(strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), err
}

func buildRunnerEnv() []string {
	env := os.Environ()
	hasBrowsersPath := false
	hasNodePath := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PLAYWRIGHT_BROWSERS_PATH=") {
			hasBrowsersPath = true
		}
		if strings.HasPrefix(kv, "NODE_PATH=") {
			hasNodePath = true
		}
	}
	if !hasBrowsersPath {
		for _, dir := range []string{"/var/cache/ms-playwright", "/opt/playwright-browsers", "/root/.cache/ms-playwright"} {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				env = append(env, "PLAYWRIGHT_BROWSERS_PATH="+dir)
				break
			}
		}
	}
	if !hasNodePath {
		var nodeDirs []string
		for _, rel := range []string{"web/node_modules", "../web/node_modules", "../../web/node_modules", "/opt/fleet/src/web/node_modules"} {
			if abs, err := filepath.Abs(rel); err == nil {
				if info, serr := os.Stat(abs); serr == nil && info.IsDir() {
					nodeDirs = append(nodeDirs, abs)
				}
			}
		}
		if len(nodeDirs) > 0 {
			env = append(env, "NODE_PATH="+strings.Join(nodeDirs, string(os.PathListSeparator)))
		}
	}
	return env
}

// startSSRFGuardProxy starts an ephemeral loopback HTTP + CONNECT proxy that
// dials every upstream address through dial (which enforces netguard.IsBlockedIP).
func startSSRFGuardProxy(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (string, func(), error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	transport := &http.Transport{
		DialContext:           dial,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	srv := &http.Server{
		ReadHeaderTimeout: 15 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				handleProxyConnect(w, r, dial)
				return
			}
			handleProxyHTTP(w, r, transport)
		}),
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			_ = ln.Close()
			_ = srv.Close()
			transport.CloseIdleConnections()
		})
	}
	return ln.Addr().String(), closeFn, nil
}

func handleProxyConnect(w http.ResponseWriter, r *http.Request, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	targetAddr := r.Host
	if _, _, err := net.SplitHostPort(targetAddr); err != nil {
		targetAddr = net.JoinHostPort(targetAddr, "443")
	}
	upConn, err := dial(r.Context(), "tcp", targetAddr)
	if err != nil {
		http.Error(w, "SSRF guard refused target", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upConn.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, buf, err := hj.Hijack()
	if err != nil {
		_ = upConn.Close()
		return
	}
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = upConn.Close()
		_ = clientConn.Close()
		return
	}
	if buf != nil && buf.Reader.Buffered() > 0 {
		if _, err := io.CopyN(upConn, buf, int64(buf.Reader.Buffered())); err != nil {
			_ = upConn.Close()
			_ = clientConn.Close()
			return
		}
	}
	go func() {
		_, _ = io.Copy(upConn, clientConn)
		_ = upConn.Close()
	}()
	go func() {
		_, _ = io.Copy(clientConn, upConn)
		_ = clientConn.Close()
	}()
}

func handleProxyHTTP(w http.ResponseWriter, r *http.Request, transport *http.Transport) {
	if r.URL == nil || (r.URL.Scheme != "http" && r.URL.Scheme != "https") || r.URL.Hostname() == "" {
		http.Error(w, "invalid proxy URL", http.StatusBadRequest)
		return
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body) //nolint:gosec // Outbound request is routed through netguard-protected transport.
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, vv := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "proxy-") {
			continue
		}
		for _, v := range vv {
			outReq.Header.Add(k, v)
		}
	}
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "SSRF guard or upstream refused request", http.StatusForbidden)
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxOutputBytes))
}

func substituteArgs(tmpl string, args map[string]any) string {
	if len(args) == 0 || !strings.Contains(tmpl, "{") {
		return tmpl
	}
	var sb strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			sb.WriteByte(tmpl[i])
			i++
			continue
		}
		// Leave {{USERNAME}} / {{PASSWORD}} / {{SECRET}} intact for the runner.
		if i+1 < len(tmpl) && tmpl[i+1] == '{' {
			end := strings.Index(tmpl[i:], "}}")
			if end >= 0 {
				sb.WriteString(tmpl[i : i+end+2])
				i += end + 2
				continue
			}
		}
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			sb.WriteString(tmpl[i:])
			break
		}
		key := tmpl[i+1 : i+end]
		if v, ok := args[key]; ok && isTokenIdent(key) {
			_, _ = fmt.Fprintf(&sb, "%v", v)
			i += end + 1
			continue
		}
		sb.WriteByte('{')
		i++
	}
	return sb.String()
}

func isTokenIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}

func scrubResult(res *RunResult, creds Credentials) {
	if res == nil {
		return
	}
	res.Content = maskSecrets(res.Content, creds)
	res.Title = maskSecrets(res.Title, creds)
	res.FinalURL = maskSecrets(res.FinalURL, creds)
	for i := range res.NetworkCalls {
		res.NetworkCalls[i].URL = maskSecrets(res.NetworkCalls[i].URL, creds)
		res.NetworkCalls[i].ResponsePreview = maskSecrets(res.NetworkCalls[i].ResponsePreview, creds)
	}
	for i := range res.Extracted {
		if s, ok := res.Extracted[i].(string); ok {
			res.Extracted[i] = maskSecrets(s, creds)
		}
	}
}

func maskSecrets(text string, creds Credentials) string {
	if text == "" {
		return ""
	}
	var forms []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && len(s) >= 3 && !seen[s] {
			seen[s] = true
			forms = append(forms, s)
		}
	}
	var rawSecrets []string
	if creds.Password != "" {
		rawSecrets = append(rawSecrets, creds.Password)
	}
	if creds.Secret != "" {
		rawSecrets = append(rawSecrets, creds.Secret)
	}
	for _, v := range creds.Headers {
		if v != "" {
			rawSecrets = append(rawSecrets, v)
		}
	}
	for _, sec := range rawSecrets {
		for _, base := range []string{sec, strings.TrimSpace(sec)} {
			if base == "" {
				continue
			}
			add(base)
			add(url.QueryEscape(base))
			add(strings.ReplaceAll(url.QueryEscape(base), "+", "%20"))
			add(url.PathEscape(base))
			if b, err := json.Marshal(base); err == nil && len(b) >= 2 {
				add(string(b[1 : len(b)-1]))
			}
		}
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, f := range forms {
		text = strings.ReplaceAll(text, f, "[REDACTED]")
	}
	return text
}

type cappedBuffer struct {
	buf bytes.Buffer
	cap int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	rem := c.cap - c.buf.Len()
	if rem <= 0 {
		return origLen, nil
	}
	if len(p) > rem {
		p = p[:rem]
	}
	_, _ = c.buf.Write(p)
	return origLen, nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

func (c *cappedBuffer) String() string {
	s := c.buf.String()
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

const embeddedPythonRunner = `
import json
import os
import shutil
import sys

def replace_secrets(val, creds):
    if not isinstance(val, str):
        return val
    u = creds.get("username") or ""
    p = creds.get("password") or ""
    s = creds.get("secret") or p or ""
    val = val.replace("{{USERNAME}}", u).replace("{{PASSWORD}}", p).replace("{{SECRET}}", s)
    return val

def find_chromium_executable():
    candidates = [
        os.environ.get("FLEET_CHROMIUM_EXECUTABLE", ""),
        "/usr/lib64/chromium-browser/headless_shell",
        "/usr/lib64/chromium-browser/chromium-browser",
        shutil.which("chromium-headless-shell") or "",
        shutil.which("headless_shell") or "",
        shutil.which("chromium") or "",
        shutil.which("chromium-browser") or "",
        shutil.which("google-chrome") or "",
        shutil.which("google-chrome-stable") or "",
    ]
    for c in candidates:
        if c and os.path.isfile(c) and os.access(c, os.X_OK):
            return c
    return None

def main():
    try:
        payload = json.load(sys.stdin)
    except Exception as exc:
        print(json.dumps({"ok": False, "error": f"invalid runner input: {exc}"}))
        return

    try:
        from playwright.sync_api import sync_playwright
    except Exception as exc:
        print(json.dumps({"ok": False, "error": f"playwright python package is not available: {exc}"}))
        return

    target_url = payload.get("url") or ""
    steps = payload.get("steps") or []
    creds = payload.get("credentials") or {}
    capture_net = bool(payload.get("capture_network"))
    inspect_dom = bool(payload.get("inspect_dom"))
    proxy_server = payload.get("proxy_server") or ""
    timeout_ms = min(max(int(payload.get("timeout_ms") or 30000), 5000), 85000)
    step_default_ms = min(15000, timeout_ms // 2)

    launch_args = [
        "--no-sandbox",
        "--disable-setuid-sandbox",
        "--disable-dev-shm-usage",
        "--disable-gpu",
        "--disable-extensions",
        "--disable-background-networking",
        "--disable-sync",
        "--no-first-run",
    ]
    if proxy_server:
        launch_args.append(f"--proxy-server={proxy_server}")
        launch_args.append("--proxy-bypass-list=<-loopback>")

    network_calls = []
    extracted = []

    try:
        with sync_playwright() as p:
            launch_kwargs = {"headless": True, "args": launch_args}
            if proxy_server:
                launch_kwargs["proxy"] = {"server": proxy_server}
            browser = None
            try:
                browser = p.chromium.launch(**launch_kwargs)
            except Exception:
                exe = find_chromium_executable()
                if not exe:
                    raise
                launch_kwargs["executable_path"] = exe
                browser = p.chromium.launch(**launch_kwargs)

            extra_headers = {}
            raw_headers = creds.get("headers") or {}
            if isinstance(raw_headers, dict):
                for hk, hv in raw_headers.items():
                    if isinstance(hk, str) and isinstance(hv, str) and hk.strip():
                        extra_headers[hk.strip()] = replace_secrets(hv, creds)

            ctx_kwargs = {
                "ignore_https_errors": False,
                "user_agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
            }
            if extra_headers:
                ctx_kwargs["extra_http_headers"] = extra_headers
            storage_state = creds.get("storage_state")
            if isinstance(storage_state, dict) and storage_state:
                ctx_kwargs["storage_state"] = storage_state

            context = browser.new_context(**ctx_kwargs)
            context.set_default_timeout(step_default_ms)

            def route_guard(route):
                req_url = route.request.url or ""
                lower = req_url.lower()
                if not (lower.startswith("https://") or lower.startswith("http://")):
                    route.abort("accessdenied")
                    return
                route.continue_()

            context.route("**/*", route_guard)

            page = context.new_page()

            if capture_net:
                def on_response(resp):
                    if len(network_calls) >= 40:
                        return
                    try:
                        req = resp.request
                        rtype = req.resource_type
                        ct = (resp.headers.get("content-type") or "").lower()
                        if rtype not in ("xhr", "fetch") and "json" not in ct and "graphql" not in req.url.lower():
                            return
                        preview = ""
                        if "json" in ct or "text" in ct:
                            try:
                                body_txt = resp.text()
                                if len(body_txt) > 600:
                                    body_txt = body_txt[:600] + "…"
                                preview = body_txt
                            except Exception:
                                preview = ""
                        network_calls.append({
                            "method": req.method,
                            "url": resp.url,
                            "status": resp.status,
                            "content_type": ct,
                            "response_preview": preview,
                        })
                    except Exception:
                        pass
                page.on("response", on_response)

            main_resp = page.goto(replace_secrets(target_url, creds), wait_until="domcontentloaded", timeout=step_default_ms)
            status_code = main_resp.status if main_resp else 0
            try:
                page.wait_for_load_state("networkidle", timeout=4000)
            except Exception:
                pass

            for st in steps:
                action = (st.get("action") or "").lower().strip()
                sel = st.get("selector") or ""
                val = replace_secrets(st.get("value") or "", creds)
                expr = replace_secrets(st.get("expression") or "", creds)
                mode = (st.get("mode") or "text").lower().strip()
                attr = st.get("attribute") or ""
                st_timeout = int(st.get("timeout_ms") or step_default_ms)
                st_timeout = min(max(st_timeout, 500), 30000)
                optional = bool(st.get("optional"))

                try:
                    if action == "goto":
                        dest = val or sel
                        r = page.goto(dest, wait_until="domcontentloaded", timeout=st_timeout)
                        if r:
                            status_code = r.status
                    elif action == "fill":
                        page.locator(sel).first.fill(val, timeout=st_timeout)
                    elif action == "click":
                        page.locator(sel).first.click(timeout=st_timeout)
                        try:
                            page.wait_for_load_state("domcontentloaded", timeout=3000)
                        except Exception:
                            pass
                    elif action == "press":
                        page.locator(sel).first.press(val, timeout=st_timeout)
                    elif action == "select":
                        page.locator(sel).first.select_option(val, timeout=st_timeout)
                    elif action == "wait_for":
                        if sel:
                            page.locator(sel).first.wait_for(state="visible", timeout=st_timeout)
                        elif val == "networkidle":
                            page.wait_for_load_state("networkidle", timeout=st_timeout)
                        elif val:
                            page.wait_for_url(val, timeout=st_timeout)
                        else:
                            page.wait_for_timeout(min(st_timeout, 10000))
                    elif action == "sleep":
                        page.wait_for_timeout(min(st_timeout, 10000))
                    elif action == "extract":
                        loc = page.locator(sel)
                        count = loc.count()
                        if mode == "table_json":
                            tables = loc.evaluate_all("""els => els.map(table => {
                                const rows = Array.from(table.querySelectorAll('tr'));
                                return rows.slice(0, 200).map(r => Array.from(r.querySelectorAll('th,td')).map(c => (c.innerText || '').trim()));
                            })""")
                            extracted.append(tables)
                        elif mode == "html":
                            if count > 0:
                                extracted.append(loc.first.inner_html(timeout=st_timeout)[:32768])
                        elif mode == "attribute" and attr:
                            if count > 0:
                                extracted.append(loc.first.get_attribute(attr, timeout=st_timeout))
                        else:
                            texts = loc.all_inner_texts()
                            extracted.append("\n".join(t.strip() for t in texts if t.strip())[:32768])
                    elif action == "eval_js":
                        code = expr or val
                        res_js = page.evaluate(code)
                        extracted.append(res_js)
                except Exception as step_exc:
                    if not optional:
                        raise RuntimeError(f"step '{action}' ({sel}) failed: {step_exc}") from step_exc

            dom_elements = []
            if inspect_dom:
                try:
                    dom_elements = page.evaluate("""() => {
                        const out = [];
                        const els = document.querySelectorAll('input, button, select, textarea, a[href], form, table');
                        for (const el of els) {
                            if (out.length >= 60) break;
                            const tag = el.tagName.toLowerCase();
                            const id = el.id || '';
                            const name = el.getAttribute('name') || '';
                            const type = el.getAttribute('type') || '';
                            const placeholder = el.getAttribute('placeholder') || '';
                            const text = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim().slice(0, 80);
                            let selector = tag;
                            if (id) selector = '#' + CSS.escape(id);
                            else if (name) selector = tag + '[name="' + name.replace(/"/g, '\\"') + '"]';
                            else if (type && tag === 'input') selector = 'input[type="' + type.replace(/"/g, '\\"') + '"]';
                            else if (text && (tag === 'button' || tag === 'a')) selector = tag + ':has-text("' + text.slice(0, 40).replace(/"/g, '\\"') + '")';
                            out.push({ tag, type, name, id, placeholder, text, selector });
                        }
                        return out;
                    }""")
                except Exception:
                    dom_elements = []

            try:
                body_text = page.locator("body").inner_text(timeout=5000)
            except Exception:
                body_text = ""
            if len(body_text) > 32768:
                body_text = body_text[:32768] + "\n…[truncated]"

            exported_state = None
            if payload.get("export_state"):
                try:
                    exported_state = context.storage_state()
                except Exception:
                    exported_state = None

            result = {
                "final_url": page.url,
                "title": page.title(),
                "status": status_code,
                "content": body_text.strip(),
                "extracted": extracted,
                "dom_elements": dom_elements,
                "network_calls": network_calls,
                "storage_state": exported_state,
            }
            context.close()
            browser.close()
            print(json.dumps({"ok": True, "result": result}))
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))

if __name__ == "__main__":
    main()
`

const embeddedNodeRunner = `
const fs = require("fs");
const path = require("path");

function replaceSecrets(val, creds) {
  if (typeof val !== "string") return val;
  const u = (creds && creds.username) || "";
  const p = (creds && creds.password) || "";
  const s = (creds && creds.secret) || p || "";
  return val.split("{{USERNAME}}").join(u).split("{{PASSWORD}}").join(p).split("{{SECRET}}").join(s);
}

function findChromiumExecutable() {
  const candidates = [
    process.env.FLEET_CHROMIUM_EXECUTABLE || "",
    "/usr/lib64/chromium-browser/headless_shell",
    "/usr/lib64/chromium-browser/chromium-browser",
    "/usr/bin/chromium-headless-shell",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/usr/bin/google-chrome",
    "/usr/bin/google-chrome-stable",
  ];
  for (const c of candidates) {
    if (!c) continue;
    try {
      fs.accessSync(c, fs.constants.X_OK);
      return c;
    } catch (_) {}
  }
  return null;
}

function loadPlaywright() {
  const searchPaths = [
    ...(process.env.NODE_PATH ? process.env.NODE_PATH.split(path.delimiter) : []),
    path.resolve(process.cwd(), "web/node_modules"),
    path.resolve(process.cwd(), "../web/node_modules"),
    path.resolve(process.cwd(), "../../web/node_modules"),
    "/opt/fleet/src/web/node_modules",
  ];
  for (const modName of ["playwright", "playwright-core"]) {
    try {
      const resolved = require.resolve(modName, { paths: searchPaths });
      return require(resolved);
    } catch (_) {}
  }
  throw new Error("playwright node package is not available");
}

(async () => {
  let payload;
  try {
    const raw = fs.readFileSync(0, "utf8");
    payload = JSON.parse(raw);
  } catch (err) {
    process.stdout.write(JSON.stringify({ ok: false, error: "invalid runner input: " + err.message }) + "\n");
    return;
  }

  let pw;
  try {
    pw = loadPlaywright();
  } catch (err) {
    process.stdout.write(JSON.stringify({ ok: false, error: err.message }) + "\n");
    return;
  }

  const targetUrl = payload.url || "";
  const steps = Array.isArray(payload.steps) ? payload.steps : [];
  const creds = payload.credentials || {};
  const captureNet = Boolean(payload.capture_network);
  const inspectDom = Boolean(payload.inspect_dom);
  const exportState = Boolean(payload.export_state);
  const proxyServer = payload.proxy_server || "";
  const timeoutMs = Math.min(Math.max(Number(payload.timeout_ms) || 30000, 5000), 85000);
  const stepDefaultMs = Math.min(15000, Math.floor(timeoutMs / 2));

  const launchArgs = [
    "--no-sandbox",
    "--disable-setuid-sandbox",
    "--disable-dev-shm-usage",
    "--disable-gpu",
    "--disable-extensions",
    "--disable-background-networking",
    "--disable-sync",
    "--no-first-run",
  ];
  if (proxyServer) {
    launchArgs.push("--proxy-server=" + proxyServer);
    launchArgs.push("--proxy-bypass-list=<-loopback>");
  }

  const networkCalls = [];
  const extracted = [];
  let browser;
  try {
    const launchOpts = { headless: true, args: launchArgs };
    if (proxyServer) {
      launchOpts.proxy = { server: proxyServer };
    }
    try {
      browser = await pw.chromium.launch(launchOpts);
    } catch (_) {
      const exe = findChromiumExecutable();
      if (!exe) throw _;
      launchOpts.executablePath = exe;
      browser = await pw.chromium.launch(launchOpts);
    }

    const extraHeaders = {};
    if (creds.headers && typeof creds.headers === "object") {
      for (const [hk, hv] of Object.entries(creds.headers)) {
        if (typeof hk === "string" && typeof hv === "string" && hk.trim()) {
          extraHeaders[hk.trim()] = replaceSecrets(hv, creds);
        }
      }
    }

    const ctxOpts = {
      ignoreHTTPSErrors: false,
      userAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
    };
    if (Object.keys(extraHeaders).length > 0) {
      ctxOpts.extraHTTPHeaders = extraHeaders;
    }
    if (creds.storage_state && typeof creds.storage_state === "object" && Object.keys(creds.storage_state).length > 0) {
      ctxOpts.storageState = creds.storage_state;
    }

    const context = await browser.newContext(ctxOpts);
    context.setDefaultTimeout(stepDefaultMs);

    await context.route("**/*", async (route) => {
      const reqUrl = (route.request().url() || "").toLowerCase();
      if (!reqUrl.startsWith("https://") && !reqUrl.startsWith("http://")) {
        await route.abort("accessdenied");
        return;
      }
      await route.continue();
    });

    const page = await context.newPage();

    if (captureNet) {
      page.on("response", async (resp) => {
        if (networkCalls.length >= 40) return;
        try {
          const req = resp.request();
          const rtype = req.resourceType();
          const headers = resp.headers() || {};
          const ct = (headers["content-type"] || "").toLowerCase();
          if (rtype !== "xhr" && rtype !== "fetch" && !ct.includes("json") && !req.url().toLowerCase().includes("graphql")) {
            return;
          }
          let preview = "";
          if (ct.includes("json") || ct.includes("text")) {
            try {
              let txt = await resp.text();
              if (txt.length > 600) txt = txt.slice(0, 600) + "…";
              preview = txt;
            } catch (_) {}
          }
          networkCalls.push({
            method: req.method(),
            url: resp.url(),
            status: resp.status(),
            content_type: ct,
            response_preview: preview,
          });
        } catch (_) {}
      });
    }

    const mainResp = await page.goto(replaceSecrets(targetUrl, creds), { waitUntil: "domcontentloaded", timeout: stepDefaultMs });
    let statusCode = mainResp ? mainResp.status() : 0;
    try {
      await page.waitForLoadState("networkidle", { timeout: 4000 });
    } catch (_) {}

    for (const st of steps) {
      const action = String(st.action || "").toLowerCase().trim();
      const sel = st.selector || "";
      const val = replaceSecrets(st.value || "", creds);
      const expr = replaceSecrets(st.expression || "", creds);
      const mode = String(st.mode || "text").toLowerCase().trim();
      const attr = st.attribute || "";
      const stTimeout = Math.min(Math.max(Number(st.timeout_ms) || stepDefaultMs, 500), 30000);
      const optional = Boolean(st.optional);

      try {
        if (action === "goto") {
          const dest = val || sel;
          const r = await page.goto(dest, { waitUntil: "domcontentloaded", timeout: stTimeout });
          if (r) statusCode = r.status();
        } else if (action === "fill") {
          await page.locator(sel).first().fill(val, { timeout: stTimeout });
        } else if (action === "click") {
          await page.locator(sel).first().click({ timeout: stTimeout });
          try {
            await page.waitForLoadState("domcontentloaded", { timeout: 3000 });
          } catch (_) {}
        } else if (action === "press") {
          await page.locator(sel).first().press(val, { timeout: stTimeout });
        } else if (action === "select") {
          await page.locator(sel).first().selectOption(val, { timeout: stTimeout });
        } else if (action === "wait_for") {
          if (sel) {
            await page.locator(sel).first().waitFor({ state: "visible", timeout: stTimeout });
          } else if (val === "networkidle") {
            await page.waitForLoadState("networkidle", { timeout: stTimeout });
          } else if (val) {
            await page.waitForURL(val, { timeout: stTimeout });
          } else {
            await page.waitForTimeout(Math.min(stTimeout, 10000));
          }
        } else if (action === "sleep") {
          await page.waitForTimeout(Math.min(stTimeout, 10000));
        } else if (action === "extract") {
          const loc = page.locator(sel);
          const count = await loc.count();
          if (mode === "table_json") {
            const tables = await loc.evaluateAll((els) =>
              els.map((table) => {
                const rows = Array.from(table.querySelectorAll("tr"));
                return rows.slice(0, 200).map((r) => Array.from(r.querySelectorAll("th,td")).map((c) => (c.innerText || "").trim()));
              })
            );
            extracted.push(tables);
          } else if (mode === "html") {
            if (count > 0) {
              const html = await loc.first().innerHTML({ timeout: stTimeout });
              extracted.push(html.slice(0, 32768));
            }
          } else if (mode === "attribute" && attr) {
            if (count > 0) {
              extracted.push(await loc.first().getAttribute(attr, { timeout: stTimeout }));
            }
          } else {
            const texts = await loc.allInnerTexts();
            extracted.push(texts.map((t) => t.trim()).filter(Boolean).join("\n").slice(0, 32768));
          }
        } else if (action === "eval_js") {
          const code = expr || val;
          const resJs = await page.evaluate(code);
          extracted.push(resJs);
        }
      } catch (stepErr) {
        if (!optional) {
          throw new Error("step '" + action + "' (" + sel + ") failed: " + stepErr.message);
        }
      }
    }

    let domElements = [];
    if (inspectDom) {
      try {
        domElements = await page.evaluate(() => {
          const out = [];
          const els = document.querySelectorAll("input, button, select, textarea, a[href], form, table");
          for (const el of els) {
            if (out.length >= 60) break;
            const tag = el.tagName.toLowerCase();
            const id = el.id || "";
            const name = el.getAttribute("name") || "";
            const type = el.getAttribute("type") || "";
            const placeholder = el.getAttribute("placeholder") || "";
            const text = (el.innerText || el.value || el.getAttribute("aria-label") || "").trim().slice(0, 80);
            let selector = tag;
            if (id) selector = "#" + CSS.escape(id);
            else if (name) selector = tag + '[name="' + name.replace(/"/g, '\\"') + '"]';
            else if (type && tag === "input") selector = 'input[type="' + type.replace(/"/g, '\\"') + '"]';
            else if (text && (tag === "button" || tag === "a")) selector = tag + ':has-text("' + text.slice(0, 40).replace(/"/g, '\\"') + '")';
            out.push({ tag, type, name, id, placeholder, text, selector });
          }
          return out;
        });
      } catch (_) {
        domElements = [];
      }
    }

    let bodyText = "";
    try {
      bodyText = await page.locator("body").innerText({ timeout: 5000 });
    } catch (_) {}
    if (bodyText.length > 32768) {
      bodyText = bodyText.slice(0, 32768) + "\n…[truncated]";
    }

    let exportedState = null;
    if (exportState) {
      try {
        exportedState = await context.storageState();
      } catch (_) {}
    }

    const result = {
      final_url: page.url(),
      title: await page.title(),
      status: statusCode,
      content: bodyText.trim(),
      extracted: extracted,
      dom_elements: domElements,
      network_calls: networkCalls,
      storage_state: exportedState,
    };
    await context.close();
    await browser.close();
    process.stdout.write(JSON.stringify({ ok: true, result }) + "\n");
  } catch (err) {
    if (browser) {
      try {
        await browser.close();
      } catch (_) {}
    }
    process.stdout.write(JSON.stringify({ ok: false, error: err && err.message ? err.message : String(err) }) + "\n");
  }
})();
`
