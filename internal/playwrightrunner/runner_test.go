package playwrightrunner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunAndLiveSessionWithLocalPlaywright(t *testing.T) {
	// Allow loopback strictly for the local httptest server in this unit test.
	origDial := DialContext
	DialContext = guardedDialContext(func(_ net.IP) bool { return false })
	defer func() { DialContext = origDial }()

	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html>
<html>
<head><title>Portal Login</title></head>
<body>
  <form id="login-form" onsubmit="event.preventDefault(); document.cookie='sid=authenticated-token; path=/'; fetch('/api/v1/metrics').then(r=>r.json()).then(d => { document.getElementById('out').innerText = 'Revenue: ' + d.revenue; });">
    <input id="user" name="username" type="text" placeholder="Email" />
    <input id="pass" name="password" type="password" placeholder="Password" />
    <button id="submit-btn" type="submit">Sign In</button>
  </form>
  <div id="out">Waiting</div>
  <table id="metrics-table">
    <tr><th>Metric</th><th>Value</th></tr>
    <tr><td>ARR</td><td>$1.2M</td></tr>
  </table>
</body>
</html>`)
	})
	mux.HandleFunc("/api/v1/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"revenue":"$100k","secret_echo":"super-secret-pass-999"}`)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	res, err := Run(ctx, RunRequest{
		URL: srv.URL + "/login",
		Credentials: Credentials{
			Username: "alice@example.com",
			Password: "super-secret-pass-999",
		},
		Steps: []Step{
			{Action: "fill", Selector: "#user", Value: "{{USERNAME}}"},
			{Action: "fill", Selector: "#pass", Value: "{{PASSWORD}}"},
			{Action: "click", Selector: "#submit-btn"},
			{Action: "wait_for", TimeoutMs: 500},
			{Action: "extract", Selector: "#metrics-table", Mode: "table_json"},
		},
		CaptureNetwork: true,
		InspectDOM:     true,
		ExportState:    true,
	})
	if err != nil {
		if strings.Contains(err.Error(), "not available") ||
			strings.Contains(strings.ToLower(err.Error()), "executable doesn't exist") ||
			strings.Contains(strings.ToLower(err.Error()), "browsertype.launch") {
			t.Skipf("skipping live Playwright browser test when playwright/chromium is not installed in this environment: %v", err)
		}
		t.Fatalf("Run failed: %v", err)
	}
	if res.Title != "Portal Login" {
		t.Errorf("Title = %q, want Portal Login", res.Title)
	}
	if !strings.Contains(res.Content, "Revenue: $100k") {
		t.Errorf("Content = %q, expected Revenue: $100k", res.Content)
	}
	if len(res.DOMElements) == 0 {
		t.Errorf("expected DOMElements to be populated when InspectDOM=true")
	}
	if len(res.NetworkCalls) == 0 {
		t.Errorf("expected NetworkCalls to capture /api/v1/metrics")
	}
	for _, nc := range res.NetworkCalls {
		if strings.Contains(nc.ResponsePreview, "super-secret-pass-999") {
			t.Errorf("expected password to be scrubbed from NetworkCalls preview, got %q", nc.ResponsePreview)
		}
	}
	if res.StorageState == nil {
		t.Errorf("expected StorageState to be exported when ExportState=true")
	}

	// Now test Co-Piloted Live Session (SessionManager): start, receive frame, send input, snapshot with storage_state.
	mgr := NewSessionManager()
	info, err := mgr.StartSession(ctx, SessionStartRequest{
		OwnerEmail: "alice@example.com",
		URL:        srv.URL + "/login",
		InspectDOM: true,
	})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	defer func() { _ = mgr.CloseSession(info.ID, "alice@example.com", "") }()

	if _, err := mgr.GetSession(info.ID, "bob@example.com", info.ViewToken); err == nil {
		t.Fatalf("expected non-owner bob@example.com to be rejected even with viewToken")
	}
	sess, err := mgr.GetSession(info.ID, "alice@example.com", "")
	if err != nil {
		t.Fatalf("GetSession for owner failed: %v", err)
	}

	frames, unsub, err := sess.SubscribeFrames()
	if err != nil {
		t.Fatalf("SubscribeFrames: %v", err)
	}
	defer unsub()

	select {
	case f := <-frames:
		if f.Data == "" {
			t.Errorf("expected non-empty base64 JPEG frame")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for initial screencast frame")
	}

	// Execute steps in the live session and export storage_state.
	stepRes, err := sess.RunSteps(ctx, []Step{
		{Action: "fill", Selector: "#user", Value: "alice@example.com"},
		{Action: "click", Selector: "#submit-btn"},
		{Action: "wait_for", TimeoutMs: 400},
	}, true, true)
	if err != nil {
		t.Fatalf("sess.RunSteps failed: %v", err)
	}
	if !strings.Contains(stepRes.Content, "Revenue: $100k") {
		t.Errorf("live session content = %q, want Revenue: $100k", stepRes.Content)
	}
	if stepRes.StorageState == nil {
		t.Errorf("expected live session to export StorageState")
	}
}

func TestRunBlocksLoopbackSSRFByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "should never be reached")
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	res, err := Run(ctx, RunRequest{
		URL: srv.URL,
	})
	if err == nil && res != nil && strings.Contains(res.Content, "should never be reached") {
		t.Fatalf("expected SSRF guard proxy to block loopback server, got content: %q", res.Content)
	}
}

func TestValidateStepsAndScrubStructuredResult(t *testing.T) {
	if err := ValidateStep(Step{Action: "eval_js", Value: "document.cookie"}); err == nil {
		t.Fatalf("expected eval_js step action to be rejected by ValidateStep")
	}
	if err := ValidateStep(Step{Action: "goto", Value: "file:///etc/passwd"}); err == nil {
		t.Fatalf("expected non-http(s) goto step URL to be rejected by ValidateStep")
	}
	if err := ValidateStep(Step{Action: "goto", Value: "https://portal.example.com/?leak={{PASSWORD}}"}); err == nil {
		t.Fatalf("expected goto step with {{PASSWORD}} placeholder to be rejected by ValidateStep")
	}
	if err := validateStepNavigationOrigin("https://portal.example.com/login", Credentials{Password: "secret"}, Step{Action: "goto", Value: "https://attacker.example.com/collect"}); err == nil {
		t.Fatalf("expected cross-origin goto step on credentialed session to be rejected")
	}
	if err := validateStepNavigationOrigin("https://portal.example.com/login", Credentials{Password: "secret"}, Step{Action: "goto", Value: "https://portal.example.com/dashboard"}); err != nil {
		t.Fatalf("expected same-origin goto step on credentialed session to pass, got %v", err)
	}
	if err := ValidateStep(Step{Action: "fill", Selector: "#user", Value: "{{USERNAME}}"}); err != nil {
		t.Fatalf("expected valid fill step to pass ValidateStep, got %v", err)
	}

	res := &RunResult{
		FinalURL: "https://portal.example.com/callback?key=abc123",
		Title:    "Welcome abc123",
		Content:  "Token is abc123",
		Extracted: []any{
			map[string]any{
				"nested_token": "abc123",
				"encoded":      "YWJjMTIz", // base64("abc123")
			},
		},
		DOMElements: []DOMElement{
			{Tag: "input", Type: "password", Selector: "#pin", Text: "abc123", Placeholder: "abc123"},
		},
		NetworkCalls: []NetworkCall{
			{URL: "https://portal.example.com/api?pin=abc123", ResponsePreview: `{"pin":"abc123"}`},
		},
	}
	scrubResult(res, Credentials{Password: "abc123", Secret: "abc123"})

	if strings.Contains(res.FinalURL, "abc123") || strings.Contains(res.Title, "abc123") || strings.Contains(res.Content, "abc123") {
		t.Fatalf("expected top-level fields to scrub short secret 'abc123': %+v", res)
	}
	if strings.Contains(res.DOMElements[0].Text, "abc123") || strings.Contains(res.DOMElements[0].Placeholder, "abc123") {
		t.Fatalf("expected DOMElements to scrub short secret 'abc123': %+v", res.DOMElements[0])
	}
	extractedMap, _ := res.Extracted[0].(map[string]any)
	if strings.Contains(fmt.Sprint(extractedMap["nested_token"]), "abc123") || strings.Contains(fmt.Sprint(extractedMap["encoded"]), "YWJjMTIz") {
		t.Fatalf("expected nested Extracted map to scrub plaintext and base64 secret: %+v", extractedMap)
	}
	if strings.Contains(res.NetworkCalls[0].URL, "abc123") || strings.Contains(res.NetworkCalls[0].ResponsePreview, "abc123") {
		t.Fatalf("expected NetworkCalls to scrub short secret 'abc123': %+v", res.NetworkCalls[0])
	}
}
