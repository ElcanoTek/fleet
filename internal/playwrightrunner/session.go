package playwrightrunner

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	sessionIdleTTL      = 10 * time.Minute
	maxActiveSessions   = 8
	sessionViewportW    = 1280
	sessionViewportH    = 720
	maxSubscribersPerID = 6
)

// FrameEvent is a live JPEG frame and page metadata emitted by a Co-Pilot browser session.
type FrameEvent struct {
	Data      string `json:"data"` // base64-encoded JPEG
	URL       string `json:"url"`
	Title     string `json:"title"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Timestamp int64  `json:"timestamp"`
}

// InputEvent represents a live user interaction from the Co-Pilot browser viewer.
type InputEvent struct {
	// Type is one of: "click", "mousedown", "mouseup", "mousemove", "wheel",
	// "keydown", "text", "navigate", "reload", "back".
	Type       string  `json:"type"`
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	Button     string  `json:"button,omitempty"` // "left", "right", "middle"
	ClickCount int     `json:"click_count,omitempty"`
	DeltaX     float64 `json:"delta_x,omitempty"`
	DeltaY     float64 `json:"delta_y,omitempty"`
	Key        string  `json:"key,omitempty"`
	Text       string  `json:"text,omitempty"`
	URL        string  `json:"url,omitempty"`
}

// SessionStartRequest configures a new Co-Piloted live Chromium session.
type SessionStartRequest struct {
	OwnerEmail  string
	URL         string
	Steps       []Step
	Credentials Credentials
	InspectDOM  bool
}

// SessionInfo summarizes a live Co-Piloted browser session.
type SessionInfo struct {
	ID         string     `json:"session_id"`
	ViewToken  string     `json:"view_token"`
	OwnerEmail string     `json:"owner_email"`
	CurrentURL string     `json:"current_url"`
	Title      string     `json:"title"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	Result     *RunResult `json:"result,omitempty"`
}

// SessionManager manages interactive Co-Piloted Playwright Chromium sessions.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*LiveSession
	starting int
}

var defaultSessionManager = NewSessionManager()

// DefaultSessionManager returns the process-wide Co-Pilot browser session manager.
func DefaultSessionManager() *SessionManager {
	return defaultSessionManager
}

// NewSessionManager creates a new Co-Pilot browser session manager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*LiveSession),
	}
}

// LiveSession is one running headless Chromium browser with CDP screencasting
// and interactive user/agent co-pilot control.
type LiveSession struct {
	id          string
	viewToken   string
	ownerEmail  string
	baseURL     string
	credentials Credentials
	createdAt   time.Time

	mu          sync.Mutex
	lastActive  time.Time
	closed      bool
	currentURL  string
	title       string
	lastFrame   *FrameEvent
	subscribers map[chan FrameEvent]struct{}
	pending     map[string]chan workerMessage
	stdin       io.WriteCloser
	cancel      context.CancelFunc
	closeProxy  func()
	cmd         *exec.Cmd
}

type workerCommand struct {
	Cmd         string      `json:"cmd"`
	ReqID       string      `json:"req_id,omitempty"`
	URL         string      `json:"url,omitempty"`
	ProxyServer string      `json:"proxy_server,omitempty"`
	Credentials Credentials `json:"credentials,omitempty"`
	Steps       []Step      `json:"steps,omitempty"`
	InspectDOM  bool        `json:"inspect_dom,omitempty"`
	ExportState bool        `json:"export_state,omitempty"`
	Input       *InputEvent `json:"input,omitempty"`
}

type workerMessage struct {
	Event  string     `json:"event"` // "frame", "cmd_result", "error"
	ReqID  string     `json:"req_id,omitempty"`
	OK     bool       `json:"ok,omitempty"`
	Error  string     `json:"error,omitempty"`
	Data   string     `json:"data,omitempty"`
	URL    string     `json:"url,omitempty"`
	Title  string     `json:"title,omitempty"`
	Result *RunResult `json:"result,omitempty"`
}

// StartSession launches a new Co-Piloted headless Chromium session, navigates to
// req.URL, runs any initial steps, and starts streaming live frames.
func (m *SessionManager) StartSession(ctx context.Context, req SessionStartRequest) (*SessionInfo, error) {
	targetURL := strings.TrimSpace(req.URL)
	if targetURL == "" {
		return nil, errors.New("url is required to start a browser session")
	}
	for _, ph := range []string{"{{USERNAME}}", "{{PASSWORD}}", "{{SECRET}}"} {
		if strings.Contains(strings.ToUpper(targetURL), ph) {
			return nil, errors.New("url must not contain secret placeholders")
		}
	}
	for _, st := range req.Steps {
		st.Action = strings.ToLower(strings.TrimSpace(st.Action))
		if err := ValidateStep(st); err != nil {
			return nil, err
		}
		if err := validateStepNavigationOrigin(targetURL, req.Credentials, st); err != nil {
			return nil, err
		}
	}

	m.reapExpired()

	owner := strings.ToLower(strings.TrimSpace(req.OwnerEmail))
	m.mu.Lock()
	if len(m.sessions)+m.starting >= maxActiveSessions {
		// Evict only the caller's own oldest session so one user cannot terminate
		// another user's active Co-Piloted login/CAPTCHA flow.
		var oldestID string
		var oldestTime time.Time
		for id, s := range m.sessions {
			if owner == "" || s.ownerEmail != owner {
				continue
			}
			s.mu.Lock()
			la := s.lastActive
			s.mu.Unlock()
			if oldestID == "" || la.Before(oldestTime) {
				oldestID = id
				oldestTime = la
			}
		}
		if oldestID == "" {
			m.mu.Unlock()
			return nil, errors.New("live browser session capacity reached; please close an existing session or try again shortly")
		}
		old := m.sessions[oldestID]
		delete(m.sessions, oldestID)
		go old.Close()
	}
	m.starting++
	m.mu.Unlock()
	slotReleased := false
	defer func() {
		if !slotReleased {
			m.mu.Lock()
			m.starting--
			m.mu.Unlock()
		}
	}()

	sessionID, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	viewToken, err := randomHex(20)
	if err != nil {
		return nil, err
	}

	sessCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	proxyAddr, closeProxy, err := startSSRFGuardProxy(sessCtx, DialContext)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start SSRF guard proxy: %w", err)
	}

	cmd, stdin, stdout, err := startSessionWorkerProcess(sessCtx)
	if err != nil {
		closeProxy()
		cancel()
		return nil, err
	}

	now := time.Now()
	sess := &LiveSession{
		id:          sessionID,
		viewToken:   viewToken,
		ownerEmail:  strings.ToLower(strings.TrimSpace(req.OwnerEmail)),
		baseURL:     targetURL,
		credentials: req.Credentials,
		createdAt:   now,
		lastActive:  now,
		currentURL:  targetURL,
		subscribers: make(map[chan FrameEvent]struct{}),
		pending:     make(map[string]chan workerMessage),
		stdin:       stdin,
		cancel:      cancel,
		closeProxy:  closeProxy,
		cmd:         cmd,
	}

	go sess.readLoop(stdout)
	go sess.watchdog(m)

	initReqID, _ := randomHex(8)
	initRes, err := sess.sendCommand(ctx, workerCommand{
		Cmd:         "init",
		ReqID:       initReqID,
		URL:         targetURL,
		ProxyServer: "http://" + proxyAddr,
		Credentials: req.Credentials,
		Steps:       req.Steps,
		InspectDOM:  req.InspectDOM,
	}, 45*time.Second)
	if err != nil {
		sess.Close()
		return nil, err
	}

	m.mu.Lock()
	m.starting--
	slotReleased = true
	m.sessions[sessionID] = sess
	m.mu.Unlock()

	return sess.Info(initRes), nil
}

// GetSession looks up a live session by ID and validates either the owner email
// or the signed/opaque viewToken.
func (m *SessionManager) GetSession(id string, ownerEmail string, viewToken string) (*LiveSession, error) {
	m.reapExpired()
	m.mu.Lock()
	sess, ok := m.sessions[strings.TrimSpace(id)]
	m.mu.Unlock()
	if !ok || sess == nil {
		return nil, errors.New("browser session not found or expired")
	}
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if closed {
		return nil, errors.New("browser session is closed")
	}
	if !sess.Authorize(ownerEmail, viewToken) {
		return nil, errors.New("not authorized for this browser session")
	}
	sess.touch()
	return sess, nil
}

// CloseSession closes and removes a session by ID.
func (m *SessionManager) CloseSession(id string, ownerEmail string, viewToken string) error {
	sess, err := m.GetSession(id, ownerEmail, viewToken)
	if err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.sessions, sess.id)
	m.mu.Unlock()
	sess.Close()
	return nil
}

func (m *SessionManager) reapExpired() {
	now := time.Now()
	var expired []*LiveSession
	m.mu.Lock()
	for id, s := range m.sessions {
		s.mu.Lock()
		idle := now.Sub(s.lastActive) > sessionIdleTTL || s.closed
		s.mu.Unlock()
		if idle {
			expired = append(expired, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		s.Close()
	}
}

// Authorize checks whether the caller owns the session (when an owner is bound)
// or holds its viewToken for unowned sessions. When s.ownerEmail is set, the
// authenticated caller MUST match s.ownerEmail so a link or token shared in a
// multiplayer conversation never grants another user access to the owner's
// live browser session.
func (s *LiveSession) Authorize(ownerEmail string, viewToken string) bool {
	caller := strings.ToLower(strings.TrimSpace(ownerEmail))
	if s.ownerEmail != "" {
		return caller != "" && caller == s.ownerEmail
	}
	if viewToken != "" && subtle.ConstantTimeCompare([]byte(viewToken), []byte(s.viewToken)) == 1 {
		return true
	}
	return false
}

// Info returns a snapshot of the session's metadata.
func (s *LiveSession) Info(res *RunResult) *SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &SessionInfo{
		ID:         s.id,
		ViewToken:  s.viewToken,
		OwnerEmail: s.ownerEmail,
		CurrentURL: s.currentURL,
		Title:      s.title,
		CreatedAt:  s.createdAt,
		ExpiresAt:  s.lastActive.Add(sessionIdleTTL),
		Result:     res,
	}
}

// SendInput forwards a live mouse/keyboard/navigation event to the Chromium page.
func (s *LiveSession) SendInput(ctx context.Context, ev InputEvent) error {
	s.touch()
	if ev.Type == "navigate" && strings.TrimSpace(ev.URL) != "" {
		cleanURL, err := ValidateTargetURL(ev.URL, true)
		if err != nil {
			return err
		}
		ev.URL = cleanURL
	}
	reqID, _ := randomHex(8)
	_, err := s.sendCommand(ctx, workerCommand{
		Cmd:   "input",
		ReqID: reqID,
		Input: &ev,
	}, 15*time.Second)
	return err
}

// RunSteps executes additional declarative steps in the live session and returns
// the updated page inspection result (including optional storage_state).
// BaseURL returns the initial target URL of the session.
func (s *LiveSession) BaseURL() string {
	return s.baseURL
}

// UpdateCredentials merges newly supplied credentials into the session.
func (s *LiveSession) UpdateCredentials(creds Credentials) {
	s.mu.Lock()
	if creds.Username != "" {
		s.credentials.Username = creds.Username
	}
	if creds.Password != "" {
		s.credentials.Password = creds.Password
	}
	if creds.Secret != "" {
		s.credentials.Secret = creds.Secret
	}
	if len(creds.Headers) > 0 {
		s.credentials.Headers = creds.Headers
	}
	if len(creds.StorageState) > 0 {
		s.credentials.StorageState = creds.StorageState
	}
	s.mu.Unlock()
}

func (s *LiveSession) RunSteps(ctx context.Context, steps []Step, inspectDOM bool, exportState bool) (*RunResult, error) {
	s.touch()
	s.mu.Lock()
	creds := s.credentials
	currentURL := s.currentURL
	s.mu.Unlock()
	for i := range steps {
		steps[i].Action = strings.ToLower(strings.TrimSpace(steps[i].Action))
		if err := ValidateStep(steps[i]); err != nil {
			return nil, err
		}
		if err := validateStepNavigationOrigin(s.baseURL, creds, steps[i]); err != nil {
			return nil, err
		}
		if stepUsesCredentials(steps[i].Value, creds) && currentURL != "" {
			if err := validateStepNavigationOrigin(s.baseURL, creds, Step{Action: "goto", Value: currentURL}); err != nil {
				return nil, fmt.Errorf("refusing secret-bearing step %q after cross-origin navigation: %w", steps[i].Action, err)
			}
		}
	}
	reqID, _ := randomHex(8)
	return s.sendCommand(ctx, workerCommand{
		Cmd:         "steps",
		ReqID:       reqID,
		Credentials: creds,
		Steps:       steps,
		InspectDOM:  inspectDOM,
		ExportState: exportState,
	}, 45*time.Second)
}

// Snapshot inspects the current live page (content, DOM elements, network calls,
// and optionally Playwright storage_state) without navigating away.
func (s *LiveSession) Snapshot(ctx context.Context, inspectDOM bool, exportState bool) (*RunResult, error) {
	s.touch()
	reqID, _ := randomHex(8)
	return s.sendCommand(ctx, workerCommand{
		Cmd:         "snapshot",
		ReqID:       reqID,
		InspectDOM:  inspectDOM,
		ExportState: exportState,
	}, 20*time.Second)
}

// SubscribeFrames registers a channel that receives live JPEG screencast frames.
// If a recent frame is already cached, it is pushed immediately.
func (s *LiveSession) SubscribeFrames() (<-chan FrameEvent, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errors.New("browser session is closed")
	}
	if len(s.subscribers) >= maxSubscribersPerID {
		return nil, nil, errors.New("too many live viewers connected to this session")
	}
	ch := make(chan FrameEvent, 4)
	if s.lastFrame != nil {
		ch <- *s.lastFrame
	}
	s.subscribers[ch] = struct{}{}
	unsub := func() {
		s.mu.Lock()
		if _, ok := s.subscribers[ch]; ok {
			delete(s.subscribers, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
	return ch, unsub, nil
}

// Close terminates the Chromium process and cleans up the SSRF guard proxy.
func (s *LiveSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	stdin := s.stdin
	cancel := s.cancel
	closeProxy := s.closeProxy
	cmd := s.cmd
	for ch := range s.subscribers {
		delete(s.subscribers, ch)
		close(ch)
	}
	for id, ch := range s.pending {
		delete(s.pending, id)
		close(ch)
	}
	s.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cancel != nil {
		cancel()
	}
	if closeProxy != nil {
		closeProxy()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func (s *LiveSession) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *LiveSession) watchdog(m *SessionManager) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		closed := s.closed
		idle := time.Since(s.lastActive) > sessionIdleTTL
		s.mu.Unlock()
		if closed {
			return
		}
		if idle {
			m.mu.Lock()
			delete(m.sessions, s.id)
			m.mu.Unlock()
			s.Close()
			return
		}
	}
}

func (s *LiveSession) sendCommand(ctx context.Context, cmd workerCommand, timeout time.Duration) (*RunResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("browser session is closed")
	}
	replyCh := make(chan workerMessage, 1)
	if cmd.ReqID != "" {
		s.pending[cmd.ReqID] = replyCh
	}
	line, err := json.Marshal(cmd)
	if err != nil {
		delete(s.pending, cmd.ReqID)
		s.mu.Unlock()
		return nil, err
	}
	_, werr := s.stdin.Write(append(line, '\n'))
	s.mu.Unlock()
	if werr != nil {
		s.mu.Lock()
		delete(s.pending, cmd.ReqID)
		s.mu.Unlock()
		return nil, fmt.Errorf("send command to browser session: %w", werr)
	}
	if cmd.ReqID == "" {
		return nil, nil
	}

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case msg, ok := <-replyCh:
		if !ok {
			return nil, errors.New("browser session closed while waiting for response")
		}
		s.mu.Lock()
		creds := s.credentials
		s.mu.Unlock()
		if !msg.OK {
			errMsg := maskSecrets(msg.Error, creds)
			if errMsg == "" {
				errMsg = "browser session command failed"
			}
			return nil, errors.New(errMsg)
		}
		if msg.Result != nil {
			scrubResult(msg.Result, creds)
			s.mu.Lock()
			if msg.Result.FinalURL != "" {
				s.currentURL = msg.Result.FinalURL
			}
			if msg.Result.Title != "" {
				s.title = msg.Result.Title
			}
			s.mu.Unlock()
		}
		return msg.Result, nil
	case <-tctx.Done():
		s.mu.Lock()
		delete(s.pending, cmd.ReqID)
		s.mu.Unlock()
		return nil, fmt.Errorf("browser session command %q timed out", cmd.Cmd)
	}
}

func (s *LiveSession) readLoop(stdout io.ReadCloser) {
	defer func() {
		_ = stdout.Close()
		s.Close()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var msg workerMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg.Event {
		case "frame":
			if msg.Data == "" {
				continue
			}
			s.mu.Lock()
			creds := s.credentials
			ev := FrameEvent{
				Data:      msg.Data,
				URL:       maskSecrets(msg.URL, creds),
				Title:     maskSecrets(msg.Title, creds),
				Width:     sessionViewportW,
				Height:    sessionViewportH,
				Timestamp: time.Now().UnixMilli(),
			}
			if ev.URL != "" {
				s.currentURL = ev.URL
			}
			if ev.Title != "" {
				s.title = ev.Title
			}
			s.lastFrame = &ev
			for ch := range s.subscribers {
				select {
				case ch <- ev:
				default:
					// Drop stale buffered frame if viewer is slow.
					select {
					case <-ch:
					default:
					}
					select {
					case ch <- ev:
					default:
					}
				}
			}
			s.mu.Unlock()
		case "cmd_result":
			if msg.ReqID == "" {
				continue
			}
			s.mu.Lock()
			ch, ok := s.pending[msg.ReqID]
			if ok {
				delete(s.pending, msg.ReqID)
			}
			s.mu.Unlock()
			if ok && ch != nil {
				ch <- msg
				close(ch)
			}
		}
	}
}

func startSessionWorkerProcess(ctx context.Context) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	env := buildRunnerEnv()
	if pythonBin, err := exec.LookPath("python3"); err == nil {
		check := exec.CommandContext(ctx, pythonBin, "-c", "import playwright") //nolint:gosec // pythonBin resolved via LookPath, constant import check.
		check.Env = env
		if check.Run() == nil {
			return spawnWorkerCmd(ctx, pythonBin, []string{"-u", "-c", embeddedPythonSessionWorker}, env)
		}
	}
	if nodeBin, err := exec.LookPath("node"); err == nil {
		return spawnWorkerCmd(ctx, nodeBin, []string{"-e", embeddedNodeSessionWorker}, env)
	}
	return nil, nil, nil, errors.New("neither python3 playwright nor node playwright is available on this host")
}

func spawnWorkerCmd(ctx context.Context, bin string, args []string, env []string) (*exec.Cmd, io.WriteCloser, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin resolved via LookPath, script is embedded constant.
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, nil, err
	}
	return cmd, stdin, stdout, nil
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// The live-session workers launch with chromiumSessionLaunchArgs (runner.go).
var (
	embeddedPythonSessionWorker = renderLaunchArgs(embeddedPythonSessionWorkerTemplate, sessionLaunchArgsPlaceholder, chromiumSessionLaunchArgs)
	embeddedNodeSessionWorker   = renderLaunchArgs(embeddedNodeSessionWorkerTemplate, sessionLaunchArgsPlaceholder, chromiumSessionLaunchArgs)
)

const embeddedPythonSessionWorkerTemplate = `
import asyncio
import base64
import json
import os
import shutil
import sys

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

def replace_secrets(val, creds):
    if not isinstance(val, str):
        return val
    u = creds.get("username") or ""
    p = creds.get("password") or ""
    s = creds.get("secret") or p or ""
    return val.replace("{{USERNAME}}", u).replace("{{PASSWORD}}", p).replace("{{SECRET}}", s)

def step_uses_secrets(raw_val, creds):
    if not isinstance(raw_val, str) or not raw_val:
        return False
    u = creds.get("username") or ""
    p = creds.get("password") or ""
    s = creds.get("secret") or p or ""
    if "{{USERNAME}}" in raw_val and u:
        return True
    if "{{PASSWORD}}" in raw_val and p:
        return True
    if "{{SECRET}}" in raw_val and s:
        return True
    return False

def ensure_same_origin(current_url, base_origin, reason):
    if not base_origin:
        raise RuntimeError(f"refusing {reason}: missing base origin")
    import urllib.parse
    cp = urllib.parse.urlsplit(current_url or "")
    cur_origin = f"{cp.scheme.lower()}://{cp.netloc.lower()}" if cp.scheme and cp.netloc else ""
    if cur_origin != base_origin:
        raise RuntimeError(f"refusing {reason} after cross-origin navigation ({cur_origin or current_url} != {base_origin})")

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

async def inspect_page(page, context, network_calls, extracted, inspect_dom, export_state):
    dom_elements = []
    if inspect_dom:
        try:
            dom_elements = await page.evaluate("""() => {
                const out = [];
                const els = document.querySelectorAll('input, button, select, textarea, a[href], form, table');
                for (const el of els) {
                    if (out.length >= 60) break;
                    const tag = el.tagName.toLowerCase();
                    const id = el.id || '';
                    const name = el.getAttribute('name') || '';
                    const type = el.getAttribute('type') || '';
                    const placeholder = el.getAttribute('placeholder') || '';
                    const isPw = type.toLowerCase() === 'password';
                    const rawText = isPw ? (el.getAttribute('aria-label') || '') : (el.innerText || el.value || el.getAttribute('aria-label') || '');
                    const text = rawText.trim().slice(0, 80);
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
        body_text = await page.locator("body").inner_text(timeout=4000)
    except Exception:
        body_text = ""
    if len(body_text) > 32768:
        body_text = body_text[:32768] + "\n…[truncated]"

    state = None
    if export_state:
        try:
            state = await context.storage_state()
        except Exception:
            state = None

    title = ""
    try:
        title = await page.title()
    except Exception:
        pass

    return {
        "final_url": page.url,
        "title": title,
        "status": 200,
        "content": body_text.strip(),
        "extracted": list(extracted),
        "dom_elements": dom_elements,
        "network_calls": list(network_calls),
        "storage_state": state,
    }

async def run_steps(page, steps, creds, extracted, base_origin=""):
    for st in (steps or []):
        action = (st.get("action") or "").lower().strip()
        sel = st.get("selector") or ""
        raw_val = st.get("value") or ""
        if step_uses_secrets(raw_val, creds):
            ensure_same_origin(page.url, base_origin, f"secret-bearing step '{action}'")
        val = replace_secrets(raw_val, creds)
        mode = (st.get("mode") or "text").lower().strip()
        attr = st.get("attribute") or ""
        st_timeout = min(max(int(st.get("timeout_ms") or 12000), 500), 30000)
        optional = bool(st.get("optional"))
        try:
            if action == "goto":
                await page.goto((st.get("value") or st.get("selector") or "").strip(), wait_until="domcontentloaded", timeout=st_timeout)
            elif action == "fill":
                await page.locator(sel).first.fill(val, timeout=st_timeout)
            elif action == "click":
                await page.locator(sel).first.click(timeout=st_timeout)
            elif action == "press":
                await page.locator(sel).first.press(val, timeout=st_timeout)
            elif action == "select":
                await page.locator(sel).first.select_option(val, timeout=st_timeout)
            elif action == "wait_for":
                if sel:
                    await page.locator(sel).first.wait_for(state="visible", timeout=st_timeout)
                elif val == "networkidle":
                    await page.wait_for_load_state("networkidle", timeout=st_timeout)
                elif val:
                    await page.wait_for_url(val, timeout=st_timeout)
                else:
                    await page.wait_for_timeout(min(st_timeout, 10000))
            elif action == "sleep":
                await page.wait_for_timeout(min(st_timeout, 10000))
            elif action == "extract":
                loc = page.locator(sel)
                count = await loc.count()
                if mode == "table_json":
                    tables = await loc.evaluate_all("""els => els.map(table => {
                        const rows = Array.from(table.querySelectorAll('tr'));
                        return rows.slice(0, 200).map(r => Array.from(r.querySelectorAll('th,td')).map(c => (c.innerText || '').trim()));
                    })""")
                    extracted.append(tables)
                elif mode == "html":
                    if count > 0:
                        extracted.append((await loc.first.inner_html(timeout=st_timeout))[:32768])
                elif mode == "attribute" and attr:
                    if count > 0:
                        extracted.append(await loc.first.get_attribute(attr, timeout=st_timeout))
                else:
                    texts = await loc.all_inner_texts()
                    extracted.append("\n".join(t.strip() for t in texts if t.strip())[:32768])
        except Exception as exc:
            if not optional:
                raise RuntimeError(f"step '{action}' ({sel}) failed: {exc}") from exc

async def push_snapshot_frame(page):
    try:
        raw = await page.screenshot(type="jpeg", quality=70)
        title = ""
        try:
            title = await page.title()
        except Exception:
            pass
        emit({
            "event": "frame",
            "data": base64.b64encode(raw).decode("ascii"),
            "url": page.url,
            "title": title,
        })
    except Exception:
        pass

async def main():
    from playwright.async_api import async_playwright
    loop = asyncio.get_running_loop()
    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    await loop.connect_read_pipe(lambda: protocol, sys.stdin)

    first_line = await reader.readline()
    if not first_line:
        return
    init_cmd = json.loads(first_line.decode("utf-8"))
    req_id = init_cmd.get("req_id") or ""
    creds = init_cmd.get("credentials") or {}
    proxy_server = init_cmd.get("proxy_server") or ""
    import urllib.parse
    parsed_base = urllib.parse.urlsplit(replace_secrets(init_cmd.get("url") or "", creds))
    base_origin = f"{parsed_base.scheme.lower()}://{parsed_base.netloc.lower()}" if parsed_base.scheme and parsed_base.netloc else ""

    launch_args = list(__FLEET_CHROMIUM_SESSION_LAUNCH_ARGS__)
    if proxy_server:
        launch_args.append(f"--proxy-server={proxy_server}")
        launch_args.append("--proxy-bypass-list=<-loopback>")

    network_calls = []
    extracted = []

    async def launch_browser(p):
        exe = find_chromium_executable()
        last_exc = None
        for exe_candidate in ([None, exe] if exe else [None]):
            kwargs = {"headless": True, "args": list(launch_args)}
            if proxy_server:
                kwargs["proxy"] = {"server": proxy_server}
            if exe_candidate:
                kwargs["executable_path"] = exe_candidate
            try:
                return await p.chromium.launch(**kwargs)
            except Exception as exc:
                last_exc = exc
        raise RuntimeError(f"failed to launch sandboxed Chromium browser (verify unprivileged user namespaces / Chromium sandbox support on this host): {last_exc}") from last_exc

    async with async_playwright() as p:
        try:
            browser = await launch_browser(p)

            ctx_kwargs = {
                "viewport": {"width": 1280, "height": 720},
                "ignore_https_errors": False,
                "user_agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
            }
            storage_state = creds.get("storage_state")
            if isinstance(storage_state, dict) and storage_state:
                ctx_kwargs["storage_state"] = storage_state

            context = await browser.new_context(**ctx_kwargs)

            async def route_guard(route):
                u = (route.request.url or "").lower()
                if not (u.startswith("https://") or u.startswith("http://")):
                    await route.abort("accessdenied")
                    return
                await route.continue_()

            await context.route("**/*", route_guard)
            page = await context.new_page()

            def on_response(resp):
                if len(network_calls) >= 50:
                    return
                try:
                    req = resp.request
                    ct = (resp.headers.get("content-type") or "").lower()
                    if req.resource_type not in ("xhr", "fetch") and "json" not in ct and "graphql" not in req.url.lower():
                        return
                    network_calls.append({
                        "method": req.method,
                        "url": resp.url,
                        "status": resp.status,
                        "content_type": ct,
                    })
                except Exception:
                    pass
            page.on("response", on_response)

            cdp = await context.new_cdp_session(page)
            async def on_screencast_frame(params):
                try:
                    sid = params.get("sessionId")
                    if sid is not None:
                        await cdp.send("Page.screencastFrameAck", {"sessionId": sid})
                    data = params.get("data") or ""
                    if data:
                        emit({
                            "event": "frame",
                            "data": data,
                            "url": page.url,
                            "title": "",
                        })
                except Exception:
                    pass

            cdp.on("Page.screencastFrame", lambda params: asyncio.create_task(on_screencast_frame(params)))
            try:
                await cdp.send("Page.startScreencast", {
                    "format": "jpeg",
                    "quality": 72,
                    "maxWidth": 1280,
                    "maxHeight": 720,
                    "everyNthFrame": 1,
                })
            except Exception:
                pass

            await page.goto(init_cmd.get("url") or "", wait_until="domcontentloaded", timeout=20000)
            try:
                await page.wait_for_load_state("networkidle", timeout=3000)
            except Exception:
                pass
            if init_cmd.get("steps"):
                await run_steps(page, init_cmd.get("steps"), creds, extracted, base_origin)
            await push_snapshot_frame(page)
            res = await inspect_page(page, context, network_calls, extracted, bool(init_cmd.get("inspect_dom")), False)
            emit({"event": "cmd_result", "req_id": req_id, "ok": True, "result": res})
        except Exception as exc:
            emit({"event": "cmd_result", "req_id": req_id, "ok": False, "error": str(exc)})
            return

        while True:
            line = await reader.readline()
            if not line:
                break
            try:
                cmd = json.loads(line.decode("utf-8"))
            except Exception:
                continue
            cname = cmd.get("cmd") or ""
            cid = cmd.get("req_id") or ""
            if cname == "close":
                break
            try:
                if cname == "input":
                    inp = cmd.get("input") or {}
                    itype = inp.get("type") or ""
                    x = float(inp.get("x") or 0)
                    y = float(inp.get("y") or 0)
                    btn = inp.get("button") or "left"
                    cc = max(int(inp.get("click_count") or 1), 1)
                    if itype == "click":
                        await page.mouse.click(x, y, button=btn, click_count=cc)
                    elif itype == "mousedown":
                        await page.mouse.move(x, y)
                        await page.mouse.down(button=btn, click_count=cc)
                    elif itype == "mouseup":
                        await page.mouse.move(x, y)
                        await page.mouse.up(button=btn, click_count=cc)
                    elif itype == "mousemove":
                        await page.mouse.move(x, y)
                    elif itype == "wheel":
                        await page.mouse.wheel(float(inp.get("delta_x") or 0), float(inp.get("delta_y") or 0))
                    elif itype == "keydown":
                        key = inp.get("key") or ""
                        if key:
                            await page.keyboard.press(key)
                    elif itype == "text":
                        txt = inp.get("text") or ""
                        if txt:
                            await page.keyboard.insert_text(txt)
                    elif itype == "navigate":
                        u = inp.get("url") or ""
                        if u:
                            await page.goto(u, wait_until="domcontentloaded", timeout=15000)
                    elif itype == "reload":
                        await page.reload(wait_until="domcontentloaded", timeout=15000)
                    elif itype == "back":
                        await page.go_back(wait_until="domcontentloaded", timeout=15000)
                    if itype in ("click", "keydown", "text", "navigate", "reload", "back"):
                        await asyncio.sleep(0.12)
                        await push_snapshot_frame(page)
                    emit({"event": "cmd_result", "req_id": cid, "ok": True, "result": {"final_url": page.url, "title": await page.title(), "content": ""}})
                elif cname == "steps":
                    cmd_creds = cmd.get("credentials") or {}
                    if isinstance(cmd_creds, dict):
                        for ck, cv in cmd_creds.items():
                            if cv:
                                creds[ck] = cv
                    extracted.clear()
                    await run_steps(page, cmd.get("steps") or [], creds, extracted, base_origin)
                    await push_snapshot_frame(page)
                    res = await inspect_page(page, context, network_calls, extracted, bool(cmd.get("inspect_dom")), bool(cmd.get("export_state")))
                    emit({"event": "cmd_result", "req_id": cid, "ok": True, "result": res})
                elif cname == "snapshot":
                    await push_snapshot_frame(page)
                    res = await inspect_page(page, context, network_calls, extracted, bool(cmd.get("inspect_dom")), bool(cmd.get("export_state")))
                    emit({"event": "cmd_result", "req_id": cid, "ok": True, "result": res})
            except Exception as exc:
                emit({"event": "cmd_result", "req_id": cid, "ok": False, "error": str(exc)})

        try:
            await context.close()
            await browser.close()
        except Exception:
            pass

if __name__ == "__main__":
    asyncio.run(main())
`

const embeddedNodeSessionWorkerTemplate = `
const fs = require("fs");
const path = require("path");
const readline = require("readline");

function emit(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

function replaceSecrets(val, creds) {
  if (typeof val !== "string") return val;
  const u = (creds && creds.username) || "";
  const p = (creds && creds.password) || "";
  const s = (creds && creds.secret) || p || "";
  return val.split("{{USERNAME}}").join(u).split("{{PASSWORD}}").join(p).split("{{SECRET}}").join(s);
}

function stepUsesSecrets(rawVal, creds) {
  if (typeof rawVal !== "string" || !rawVal) return false;
  const u = (creds && creds.username) || "";
  const p = (creds && creds.password) || "";
  const s = (creds && creds.secret) || p || "";
  if (rawVal.includes("{{USERNAME}}") && u) return true;
  if (rawVal.includes("{{PASSWORD}}") && p) return true;
  if (rawVal.includes("{{SECRET}}") && s) return true;
  return false;
}

function ensureSameOrigin(currentUrl, baseOrigin, reason) {
  if (!baseOrigin) {
    throw new Error("refusing " + reason + ": missing base origin");
  }
  let curOrigin = "";
  try {
    curOrigin = new URL(currentUrl || "").origin.toLowerCase();
  } catch (_) {}
  if (curOrigin !== baseOrigin) {
    throw new Error("refusing " + reason + " after cross-origin navigation (" + (curOrigin || currentUrl) + " != " + baseOrigin + ")");
  }
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

async function inspectPage(page, context, networkCalls, extracted, inspectDom, exportState) {
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
          const isPw = type.toLowerCase() === "password";
          const rawText = isPw ? (el.getAttribute("aria-label") || "") : (el.innerText || el.value || el.getAttribute("aria-label") || "");
          const text = rawText.trim().slice(0, 80);
          let selector = tag;
          if (id) selector = "#" + CSS.escape(id);
          else if (name) selector = tag + '[name="' + name.replace(/"/g, '\\"') + '"]';
          else if (type && tag === "input") selector = 'input[type="' + type.replace(/"/g, '\\"') + '"]';
          else if (text && (tag === "button" || tag === "a")) selector = tag + ':has-text("' + text.slice(0, 40).replace(/"/g, '\\"') + '")';
          out.push({ tag, type, name, id, placeholder, text, selector });
        }
        return out;
      });
    } catch (_) {}
  }

  let bodyText = "";
  try {
    bodyText = await page.locator("body").innerText({ timeout: 4000 });
  } catch (_) {}
  if (bodyText.length > 32768) {
    bodyText = bodyText.slice(0, 32768) + "\n…[truncated]";
  }

  let state = null;
  if (exportState) {
    try {
      state = await context.storageState();
    } catch (_) {}
  }

  let title = "";
  try {
    title = await page.title();
  } catch (_) {}

  return {
    final_url: page.url(),
    title,
    status: 200,
    content: bodyText.trim(),
    extracted: [...extracted],
    dom_elements: domElements,
    network_calls: [...networkCalls],
    storage_state: state,
  };
}

async function runSteps(page, steps, creds, extracted, baseOrigin) {
  for (const st of steps || []) {
    const action = String(st.action || "").toLowerCase().trim();
    const sel = st.selector || "";
    const rawVal = st.value || "";
    if (stepUsesSecrets(rawVal, creds)) {
      ensureSameOrigin(page.url(), baseOrigin || "", "secret-bearing step '" + action + "'");
    }
    const val = replaceSecrets(rawVal, creds);
    const mode = String(st.mode || "text").toLowerCase().trim();
    const attr = st.attribute || "";
    const stTimeout = Math.min(Math.max(Number(st.timeout_ms) || 12000, 500), 30000);
    const optional = Boolean(st.optional);
    try {
      if (action === "goto") {
        await page.goto(String(st.value || st.selector || "").trim(), { waitUntil: "domcontentloaded", timeout: stTimeout });
      } else if (action === "fill") {
        await page.locator(sel).first().fill(val, { timeout: stTimeout });
      } else if (action === "click") {
        await page.locator(sel).first().click({ timeout: stTimeout });
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
          if (count > 0) extracted.push((await loc.first().innerHTML({ timeout: stTimeout })).slice(0, 32768));
        } else if (mode === "attribute" && attr) {
          if (count > 0) extracted.push(await loc.first().getAttribute(attr, { timeout: stTimeout }));
        } else {
          const texts = await loc.allInnerTexts();
          extracted.push(texts.map((t) => t.trim()).filter(Boolean).join("\n").slice(0, 32768));
        }
      }
    } catch (err) {
      if (!optional) {
        throw new Error("step '" + action + "' (" + sel + ") failed: " + err.message);
      }
    }
  }
}

async function pushSnapshotFrame(page) {
  try {
    const buf = await page.screenshot({ type: "jpeg", quality: 70 });
    let title = "";
    try {
      title = await page.title();
    } catch (_) {}
    emit({
      event: "frame",
      data: buf.toString("base64"),
      url: page.url(),
      title,
    });
  } catch (_) {}
}

(async () => {
  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  const lines = rl[Symbol.asyncIterator]();

  const first = await lines.next();
  if (first.done || !first.value) return;
  const initCmd = JSON.parse(first.value);
  const reqId = initCmd.req_id || "";
  const creds = initCmd.credentials || {};
  const proxyServer = initCmd.proxy_server || "";
  let baseOrigin = "";
  try {
    baseOrigin = new URL(replaceSecrets(initCmd.url || "", creds)).origin.toLowerCase();
  } catch (_) {}

  let pw;
  try {
    pw = loadPlaywright();
  } catch (err) {
    emit({ event: "cmd_result", req_id: reqId, ok: false, error: err.message });
    return;
  }

  const launchArgs = [...__FLEET_CHROMIUM_SESSION_LAUNCH_ARGS__];
  if (proxyServer) {
    launchArgs.push("--proxy-server=" + proxyServer);
    launchArgs.push("--proxy-bypass-list=<-loopback>");
  }

  async function launchBrowser() {
    const exe = findChromiumExecutable();
    let lastErr;
    for (const exeCandidate of exe ? [null, exe] : [null]) {
      const opts = { headless: true, args: [...launchArgs] };
      if (proxyServer) opts.proxy = { server: proxyServer };
      if (exeCandidate) opts.executablePath = exeCandidate;
      try {
        return await pw.chromium.launch(opts);
      } catch (err) {
        lastErr = err;
      }
    }
    throw new Error("failed to launch sandboxed Chromium browser (verify unprivileged user namespaces / Chromium sandbox support on this host): " + (lastErr && lastErr.message ? lastErr.message : String(lastErr)));
  }

  const networkCalls = [];
  const extracted = [];
  let browser;
  let context;
  let page;

  try {
    browser = await launchBrowser();

    const ctxOpts = {
      viewport: { width: 1280, height: 720 },
      ignoreHTTPSErrors: false,
      userAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
    };
    if (creds.storage_state && typeof creds.storage_state === "object" && Object.keys(creds.storage_state).length > 0) {
      ctxOpts.storageState = creds.storage_state;
    }

    context = await browser.newContext(ctxOpts);
    await context.route("**/*", async (route) => {
      const u = (route.request().url() || "").toLowerCase();
      if (!u.startsWith("https://") && !u.startsWith("http://")) {
        await route.abort("accessdenied");
        return;
      }
      await route.continue();
    });

    page = await context.newPage();
    page.on("response", (resp) => {
      if (networkCalls.length >= 50) return;
      try {
        const req = resp.request();
        const ct = ((resp.headers() || {})["content-type"] || "").toLowerCase();
        if (req.resourceType() !== "xhr" && req.resourceType() !== "fetch" && !ct.includes("json") && !req.url().toLowerCase().includes("graphql")) {
          return;
        }
        networkCalls.push({
          method: req.method(),
          url: resp.url(),
          status: resp.status(),
          content_type: ct,
        });
      } catch (_) {}
    });

    const cdp = await context.newCDPSession(page);
    cdp.on("Page.screencastFrame", async (params) => {
      try {
        if (params.sessionId !== undefined) {
          await cdp.send("Page.screencastFrameAck", { sessionId: params.sessionId });
        }
        if (params.data) {
          emit({
            event: "frame",
            data: params.data,
            url: page.url(),
            title: "",
          });
        }
      } catch (_) {}
    });
    try {
      await cdp.send("Page.startScreencast", {
        format: "jpeg",
        quality: 72,
        maxWidth: 1280,
        maxHeight: 720,
        everyNthFrame: 1,
      });
    } catch (_) {}

    await page.goto(initCmd.url || "", { waitUntil: "domcontentloaded", timeout: 20000 });
    try {
      await page.waitForLoadState("networkidle", { timeout: 3000 });
    } catch (_) {}
    if (Array.isArray(initCmd.steps) && initCmd.steps.length > 0) {
      await runSteps(page, initCmd.steps, creds, extracted, baseOrigin);
    }
    await pushSnapshotFrame(page);
    const res = await inspectPage(page, context, networkCalls, extracted, Boolean(initCmd.inspect_dom), false);
    emit({ event: "cmd_result", req_id: reqId, ok: true, result: res });
  } catch (err) {
    emit({ event: "cmd_result", req_id: reqId, ok: false, error: err && err.message ? err.message : String(err) });
    if (browser) {
      try { await browser.close(); } catch (_) {}
    }
    return;
  }

  for await (const rawLine of lines) {
    if (!rawLine) continue;
    let cmd;
    try {
      cmd = JSON.parse(rawLine);
    } catch (_) {
      continue;
    }
    const cname = cmd.cmd || "";
    const cid = cmd.req_id || "";
    if (cname === "close") break;
    try {
      if (cname === "input") {
        const inp = cmd.input || {};
        const itype = inp.type || "";
        const x = Number(inp.x) || 0;
        const y = Number(inp.y) || 0;
        const btn = inp.button || "left";
        const cc = Math.max(Number(inp.click_count) || 1, 1);
        if (itype === "click") {
          await page.mouse.click(x, y, { button: btn, clickCount: cc });
        } else if (itype === "mousedown") {
          await page.mouse.move(x, y);
          await page.mouse.down({ button: btn, clickCount: cc });
        } else if (itype === "mouseup") {
          await page.mouse.move(x, y);
          await page.mouse.up({ button: btn, clickCount: cc });
        } else if (itype === "mousemove") {
          await page.mouse.move(x, y);
        } else if (itype === "wheel") {
          await page.mouse.wheel(Number(inp.delta_x) || 0, Number(inp.delta_y) || 0);
        } else if (itype === "keydown" && inp.key) {
          await page.keyboard.press(inp.key);
        } else if (itype === "text" && inp.text) {
          await page.keyboard.insertText(inp.text);
        } else if (itype === "navigate" && inp.url) {
          await page.goto(inp.url, { waitUntil: "domcontentloaded", timeout: 15000 });
        } else if (itype === "reload") {
          await page.reload({ waitUntil: "domcontentloaded", timeout: 15000 });
        } else if (itype === "back") {
          await page.goBack({ waitUntil: "domcontentloaded", timeout: 15000 });
        }
        if (["click", "keydown", "text", "navigate", "reload", "back"].includes(itype)) {
          await new Promise((r) => setTimeout(r, 120));
          await pushSnapshotFrame(page);
        }
        let title = "";
        try { title = await page.title(); } catch (_) {}
        emit({ event: "cmd_result", req_id: cid, ok: true, result: { final_url: page.url(), title, content: "" } });
      } else if (cname === "steps") {
        if (cmd.credentials && typeof cmd.credentials === "object") {
          for (const [ck, cv] of Object.entries(cmd.credentials)) {
            if (cv) creds[ck] = cv;
          }
        }
        extracted.length = 0;
        await runSteps(page, cmd.steps || [], creds, extracted, baseOrigin);
        await pushSnapshotFrame(page);
        const res = await inspectPage(page, context, networkCalls, extracted, Boolean(cmd.inspect_dom), Boolean(cmd.export_state));
        emit({ event: "cmd_result", req_id: cid, ok: true, result: res });
      } else if (cname === "snapshot") {
        await pushSnapshotFrame(page);
        const res = await inspectPage(page, context, networkCalls, extracted, Boolean(cmd.inspect_dom), Boolean(cmd.export_state));
        emit({ event: "cmd_result", req_id: cid, ok: true, result: res });
      }
    } catch (err) {
      emit({ event: "cmd_result", req_id: cid, ok: false, error: err && err.message ? err.message : String(err) });
    }
  }

  try {
    await context.close();
    await browser.close();
  } catch (_) {}
})();
`
