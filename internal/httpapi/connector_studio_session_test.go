package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// stubSessionWorker puts a python3 on PATH that stands in for the Playwright
// live-session worker. It answers the "import playwright" probe, then replies
// to every command line on stdin with an ok cmd_result whose final_url is the
// URL the session started on, appending each command to the returned log. No
// browser and no network: the log shows exactly what a handler handed the
// worker, so these tests run in CI.
//
// Callers must not be parallel (t.Setenv).
func stubSessionWorker(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "worker-commands.jsonl")
	script := `#!/bin/sh
if [ "$1" = "-c" ] && [ "$2" = "import playwright" ]; then exit 0; fi
base=""
while IFS= read -r line; do
  printf '%s\n' "$line" >> '` + logPath + `'
  id=$(printf '%s' "$line" | sed -n 's/.*"req_id":"\([^"]*\)".*/\1/p')
  case "$line" in
    '{"cmd":"init"'*) base=$(printf '%s' "$line" | sed -n 's/.*"url":"\([^"]*\)".*/\1/p') ;;
  esac
  if [ -n "$id" ]; then
    printf '{"event":"cmd_result","req_id":"%s","ok":true,"result":{"final_url":"%s","title":"stub"}}\n' "$id" "$base"
  fi
done
`
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub worker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// workerCommands reads the commands the stub worker received.
func workerCommands(t *testing.T, logPath string) []map[string]any {
	t.Helper()
	f, err := os.Open(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open worker log: %v", err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			t.Fatalf("worker log line %q: %v", sc.Text(), err)
		}
		out = append(out, cmd)
	}
	return out
}

// newLiveSessionTestServer is a Server with a vaulted password confirmed for
// exactly one origin.
func newLiveSessionTestServer(t *testing.T, user, password, confirmedOrigin string, allowInsecureHTTP bool) (*Server, *store.Conversation) {
	t.Helper()
	chatSt := newFakeChatStore()
	conv, err := chatSt.CreateConversation(context.Background(), user, "Build Connector", "default", "", false)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	s := &Server{
		store: chatSt,
		remoteMCP: remotemcp.NewService(newMemRemoteMCPStore(), remotemcp.Config{
			PublicBaseURL:     "https://fleet.example.com",
			AllowInsecureHTTP: allowInsecureHTTP,
		}),
	}
	s.uiSecrets.put(user, conv.ID, "password", password)
	s.uiSecrets.allowOrigin(user, conv.ID, confirmedOrigin)
	return s, conv
}

// startTestCopilotSession starts a live session through the handler. Only a
// real-browser test may skip (allowSkip): a stub-backed test that cannot start
// its session must fail, or a stub that never took over would pass as a skip.
func startTestCopilotSession(ctx context.Context, t *testing.T, h tools.ConnectorStudioHandler, user, url string, allowSkip bool) string {
	t.Helper()
	started, err := h(ctx, tools.ConnectorStudioParams{Action: tools.ConnectorStudioActionStartCopilotSession, URL: url})
	if err != nil {
		if reason := liveBrowserSkipReason(err); allowSkip && reason != "" {
			t.Skipf("skipping: %s: %v", reason, err)
		}
		t.Fatalf("start_copilot_session: %v", err)
	}
	sessionID, _ := started.(map[string]any)["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("start_copilot_session returned no session_id: %#v", started)
	}
	t.Cleanup(func() { _ = playwrightrunner.DefaultSessionManager().CloseSession(sessionID, user, "") })
	return sessionID
}

// liveBrowserSkipReason says why a live browser test should skip rather than
// fail: Playwright or Chromium is missing, or Chromium's sandbox cannot start
// on this host. Any other error is a real failure and returns "".
func liveBrowserSkipReason(err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not available"), strings.Contains(lower, "neither python3 playwright nor node playwright"),
		strings.Contains(lower, "executable doesn't exist"), strings.Contains(lower, "host system is missing dependencies"):
		return "Playwright or Chromium is not installed"
	case strings.Contains(msg, "No usable sandbox"), strings.Contains(msg, "sys_chroot"),
		strings.Contains(msg, "Zygote process exited prematurely"), strings.Contains(lower, "running as root without --no-sandbox"):
		return "Chromium's sandbox cannot start on this host"
	}
	return ""
}

// TestConnectorStudioLiveSessionHandsStepsOverWithPlaceholders pins what
// probe_browser sends a live session: the steps keep their {{PASSWORD}}
// placeholder, and the password travels only in the credentials, so the
// worker can refuse it on any origin but the session's. Runs against the stub
// worker, so it runs in CI.
func TestConnectorStudioLiveSessionHandsStepsOverWithPlaceholders(t *testing.T) {
	const (
		user   = "admin@example.com"
		canary = "canary-pw-1928-placeholder"
	)
	logPath := stubSessionWorker(t)
	s, conv := newLiveSessionTestServer(t, user, canary, "https://a.example.com", false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := s.newConnectorStudioHandler(conv, user)
	sessionID := startTestCopilotSession(ctx, t, h, user, "https://a.example.com/login", false)

	if _, err := h(ctx, tools.ConnectorStudioParams{
		Action:      tools.ConnectorStudioActionProbeBrowser,
		SessionID:   sessionID,
		PasswordRef: "$secret:password",
		Steps:       []playwrightrunner.Step{{Action: "fill", Selector: "#pw", Value: "{{PASSWORD}}"}},
	}); err != nil {
		t.Fatalf("probe_browser on the session: %v", err)
	}

	cmds := workerCommands(t, logPath)
	if len(cmds) == 0 || cmds[0]["cmd"] != "init" {
		t.Fatalf("the stub worker did not start the session (commands %v)", cmds)
	}
	var steps []map[string]any
	for _, cmd := range cmds {
		if cmd["cmd"] == "steps" {
			steps = append(steps, cmd)
		}
	}
	if len(steps) != 1 {
		t.Fatalf("the worker received %d steps commands, want 1", len(steps))
	}
	first := steps[0]["steps"].([]any)[0].(map[string]any)
	if got := first["value"]; got != "{{PASSWORD}}" {
		t.Errorf("the fill step reached the worker as %q, want the {{PASSWORD}} placeholder", got)
	}
	if got := steps[0]["credentials"].(map[string]any)["password"]; got != canary {
		t.Errorf("the steps command carries password %q, want the vaulted one", got)
	}
}

// TestLiveSessionCredentialsResolveForTheSessionOrigin: a secret confirmed
// for origin A must not be bound to a live session started on origin B, even
// when the call names A as its url. Otherwise the worker, which compares
// against the session's own origin, would fill it on B. Covers both tools that
// drive live sessions; runs against the stub worker, so it runs in CI.
func TestLiveSessionCredentialsResolveForTheSessionOrigin(t *testing.T) {
	const (
		user   = "admin@example.com"
		canary = "canary-pw-1928-other-session"
	)
	logPath := stubSessionWorker(t)
	s, conv := newLiveSessionTestServer(t, user, canary, "https://a.example.com", false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := s.newConnectorStudioHandler(conv, user)
	sessionID := startTestCopilotSession(ctx, t, h, user, "https://b.example.com/form", false)
	fill := []playwrightrunner.Step{{Action: "fill", Selector: "#pw", Value: "{{PASSWORD}}"}}

	_, err := h(ctx, tools.ConnectorStudioParams{
		Action:      tools.ConnectorStudioActionProbeBrowser,
		SessionID:   sessionID,
		URL:         "https://a.example.com/login",
		PasswordRef: "$secret:password",
		Steps:       fill,
	})
	if err == nil || !strings.Contains(err.Error(), "not authorized for host") {
		t.Fatalf("connector_studio probe_browser with url=A on a session started on B: err = %v, want the secret refused for B", err)
	}
	_, err = tools.RunPlaywrightBrowserForApproval(ctx, s.newPlaywrightBrowserContext(conv, user), tools.PlaywrightBrowserParams{
		Action:      "steps",
		SessionID:   sessionID,
		URL:         "https://a.example.com/login",
		PasswordRef: "$secret:password",
		Steps:       fill,
	})
	if err == nil || !strings.Contains(err.Error(), "not authorized for host") {
		t.Fatalf("playwright_browser steps with url=A on a session started on B: err = %v, want the secret refused for B", err)
	}
	// A later call without credential refs must not find the password bound
	// to the session either.
	if _, err := h(ctx, tools.ConnectorStudioParams{
		Action:    tools.ConnectorStudioActionProbeBrowser,
		SessionID: sessionID,
		Steps:     fill,
	}); err != nil {
		t.Fatalf("probe_browser without refs: %v", err)
	}

	cmds := workerCommands(t, logPath)
	got := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		got = append(got, fmt.Sprint(cmd["cmd"]))
	}
	if strings.Join(got, ",") != "init,steps" {
		t.Fatalf("the stub worker received commands %v, want init then the one steps call", got)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read worker log: %v", err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("the session started on B received the password confirmed for A:\n%s", raw)
	}
}

// TestConnectorStudioLiveSessionKeepsSecretStepsOnTheSessionOrigin drives
// probe_browser on a real live Co-Pilot session. The session starts on origin
// A, where the vaulted password is confirmed:
//   - a batch that clicks through to origin B and then fills {{PASSWORD}} is
//     refused by the worker, which checks the live page before the fill;
//   - with the page already on B, the same fill is refused again: by RunSteps
//     before the batch is sent once the session has seen the page on B,
//     otherwise by the worker;
//   - back on A the steps fill and submit the password.
//
// Both checks key on the placeholder, so they only work because the handler
// hands the steps over with their placeholders intact. Skipped where
// Playwright or Chromium is not installed, or the sandbox cannot start.
//
// Not parallel: it swaps playwrightrunner.DialContext so the browser's guard
// proxy admits the two loopback test servers.
func TestConnectorStudioLiveSessionKeepsSecretStepsOnTheSessionOrigin(t *testing.T) {
	const (
		user   = "admin@example.com"
		canary = "canary-pw-1928-live-session"
	)

	// Cleanups run last-registered first, so the session (registered later,
	// in startTestCopilotSession) closes before the servers and the dial
	// override go away.
	origDial := playwrightrunner.DialContext
	playwrightrunner.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	t.Cleanup(func() { playwrightrunner.DialContext = origDial })

	var mu sync.Mutex
	received := map[string][]string{}
	var linkToB string
	newSite := func(name string) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/form", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			link := linkToB
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!doctype html><title>%s</title><p id="site-%s">%s</p><a id="away" href="%s">away</a>
<form action="/echo" method="get"><input id="pw" name="pw"><button id="go" type="submit">Go</button></form>`, name, name, name, link)
		})
		mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			received[name] = append(received[name], r.URL.Query().Get("pw"))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!doctype html><title>%s echo</title><p id="done">received</p>`, name)
		})
		return httptest.NewServer(mux)
	}
	siteA, siteB := newSite("A"), newSite("B")
	t.Cleanup(siteA.Close)
	t.Cleanup(siteB.Close)
	mu.Lock()
	linkToB = siteB.URL + "/form"
	mu.Unlock()

	// The test servers are plain http://127.0.0.1.
	s, conv := newLiveSessionTestServer(t, user, canary, siteA.URL, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h := s.newConnectorStudioHandler(conv, user)
	sessionID := startTestCopilotSession(ctx, t, h, user, siteA.URL+"/form", true)

	secretSteps := func() []playwrightrunner.Step {
		return []playwrightrunner.Step{
			{Action: "fill", Selector: "#pw", Value: "{{PASSWORD}}"},
			{Action: "click", Selector: "#go"},
			{Action: "wait_for", Selector: "#done"},
		}
	}
	probe := func(steps []playwrightrunner.Step) error {
		_, err := h(ctx, tools.ConnectorStudioParams{
			Action:      tools.ConnectorStudioActionProbeBrowser,
			SessionID:   sessionID,
			PasswordRef: "$secret:password",
			Steps:       steps,
		})
		return err
	}
	expectRefused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "cross-origin") {
			t.Fatalf("%s: err = %v, want a cross-origin refusal", what, err)
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("%s: the refusal contains the secret: %v", what, err)
		}
	}

	// 1. Leave A within the batch: only the worker sees the page on B.
	expectRefused("click through to B, then fill", probe(append([]playwrightrunner.Step{
		{Action: "click", Selector: "#away"},
		{Action: "wait_for", Selector: "#site-B"},
	}, secretSteps()...)))

	// 2. The page is now on B before the batch starts.
	expectRefused("fill while the page is on B", probe(secretSteps()))

	// 3. Back on A (an address-bar navigation, as in the live view: goto steps
	// accept only https://), the same steps fill and submit the password.
	sess, err := playwrightrunner.DefaultSessionManager().GetSession(sessionID, user, "")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if err := sess.SendInput(ctx, playwrightrunner.InputEvent{Type: "navigate", URL: siteA.URL + "/form"}); err != nil {
		t.Fatalf("navigate back to A: %v", err)
	}
	if err := probe(secretSteps()); err != nil {
		t.Fatalf("the same steps on origin A: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := received["B"]; len(got) != 0 {
		t.Errorf("origin B received a form submission %q; the secret step must not run there", got)
	}
	if got := received["A"]; len(got) != 1 || got[0] != canary {
		t.Errorf("origin A received %q, want exactly the vaulted password", got)
	}
}
