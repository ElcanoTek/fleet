// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

type nextRoute struct{ method, path string }

func readNextRoutes(t *testing.T) []nextRoute {
	t.Helper()
	f, err := os.Open("testdata/next-api-routes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []nextRoute
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m, p, _ := strings.Cut(line, " ")
		out = append(out, nextRoute{m, p})
	}
	return out
}

var dynSegment = regexp.MustCompile(`\{[^}]+\}`)

// samplePath fills every dynamic segment with a value.
func samplePath(p string) string {
	return dynSegment.ReplaceAllStringFunc(p, func(s string) string {
		if strings.HasSuffix(s, "...}") {
			return "dir/file.png"
		}
		return "x1"
	})
}

// TestEveryNextRouteIsServed is the inventory: every method+path the Next.js
// tier served is registered here (none dropped), and the gateway serves no
// /api route that tier did not (no browser surface added silently).
func TestEveryNextRouteIsServed(t *testing.T) {
	g := newTestEnv(t).g
	routes := readNextRoutes(t)
	if len(routes) < 200 {
		t.Fatalf("inventory has %d routes; the fixture is broken", len(routes))
	}
	want := map[string]bool{}
	for _, rt := range routes {
		r := httptest.NewRequest(rt.method, samplePath(rt.path), nil)
		_, pattern := g.api.Handler(r)
		if pattern == "" || pattern == "/api/" {
			t.Errorf("%s %s is not served (pattern %q)", rt.method, rt.path, pattern)
			continue
		}
		want[pattern] = true
	}
	var extra []string
	for _, pat := range registeredPatterns(g) {
		if !want[pat] && pat != "/api/" {
			extra = append(extra, pat)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("registered but not in the Next inventory: %v", extra)
	}
}

// registeredPatterns lists the API mux patterns by probing the inventory's
// own shapes plus the routes registered by name (the mux has no listing API).
func registeredPatterns(g *Gateway) []string {
	seen := map[string]bool{}
	for _, rt := range readNextRoutesNoT() {
		for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"} {
			r := httptest.NewRequest(m, samplePath(rt.path), nil)
			if _, pat := g.api.Handler(r); pat != "" && !(m == "HEAD" && strings.HasPrefix(pat, "GET ")) {
				seen[pat] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

func readNextRoutesNoT() []nextRoute {
	raw, _ := os.ReadFile("testdata/next-api-routes.txt")
	var out []nextRoute
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m, p, _ := strings.Cut(line, " ")
		out = append(out, nextRoute{m, p})
	}
	return out
}

func TestUnknownAPIIsJSON404AndWrongMethodIs405(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(signedIn(t, req("GET", "/api/does-not-exist", "")))
	body := readBody(t, res)
	if res.StatusCode != http.StatusNotFound || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") || !strings.Contains(body, `"not found"`) {
		t.Fatalf("%d %q %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	if strings.Contains(body, "<html") {
		t.Fatal("unknown /api answered with the SPA")
	}
	res = e.do(signedIn(t, req("PUT", "/api/personas", "")))
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method: %d", res.StatusCode)
	}
}

func TestChatPassthroughCarriesIdentityAndNothingFromTheBrowser(t *testing.T) {
	e := newTestEnv(t)
	tok, _ := websession.NewHMAC(testSecret).MintOIDC("dave@x.com", "ext-epoch", "https://auth.example.com", "sub-1")
	r := withCookie(req("POST", "/api/conversations/c%2F1/rename", `{"title":"t"}`,
		"Origin", "http://fleet.example.com",
		"X-Chat-Server-Token", "attacker",
		"X-User-Email", "victim@x.com",
		"X-Forwarded-For", "203.0.113.9",
		"Content-Type", "text/plain",
	), websession.CookieName, tok)
	res := e.do(r)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	got := e.chat.last(t)
	if got.Method != "POST" || got.URI != "/conversations/c%2F1/rename" || got.Path != "/conversations/c/1/rename" {
		t.Fatalf("upstream %s %q path %q", got.Method, got.URI, got.Path)
	}
	h := got.Header
	if h.Get("X-Chat-Server-Token") != testChatToken || h.Get("X-User-Email") != "dave@x.com" || h.Get("X-User-Session-Epoch") != "ext-epoch" ||
		h.Get("X-User-Session-Source") != "oidc" || h.Get("X-External-Issuer") != "https://auth.example.com" || h.Get("X-External-Subject") != "sub-1" {
		t.Fatalf("identity headers %v", h)
	}
	if h.Get("X-Forwarded-For") != "" || h.Get("Origin") != "" || h.Get("Cookie") != "" || got.RemoteAddr != backendRemoteAddr {
		t.Fatalf("browser headers leaked upstream: %v remote %q", h, got.RemoteAddr)
	}
	if h.Get("Content-Type") != "application/json" || got.Body != `{"title":"t"}` {
		t.Fatalf("body %q ct %q", got.Body, h.Get("Content-Type"))
	}
}

func TestPasswordSessionCarriesNoOIDCHeaders(t *testing.T) {
	e := newTestEnv(t)
	e.do(signedIn(t, req("GET", "/api/personas", "")))
	h := e.chat.last(t).Header
	if h.Get("X-User-Session-Epoch") != "epoch-1" || h.Get("X-User-Session-Source") != "" || h.Get("X-External-Issuer") != "" {
		t.Fatalf("%v", h)
	}
	// An elcano_auth session has no epoch to forward.
	e.do(withCookie(req("GET", "/api/personas", ""), "elcano_auth", e.mintElcano(t, "Carol@x.com")))
	h = e.chat.last(t).Header
	if h.Get("X-User-Email") != "carol@x.com" || h.Get("X-User-Session-Epoch") != "" {
		t.Fatalf("%v", h)
	}
}

func TestQueryForwardingIsPerRoute(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct{ target, want string }{
		{"/api/conversations?archived=true&x=1", "/conversations?archived=true"},
		{"/api/conversations?scope=team&archived=true", "/conversations?scope=team"},
		{"/api/conversations?evil=1", "/conversations"},
		{"/api/search?q=hello%20world&limit=5", "/search?q=hello%20world&limit=5"},
		{"/api/admin/doctor?deep=1", "/admin/doctor?deep=1"},
		{"/api/admin/doctor?deep=2", "/admin/doctor"},
		{"/api/conversations/c1/approvals/a%201", "/conversations/c1?approval_id=a%201"},
		{"/api/conversations/c1/export?format=html&include=full&x=1", "/conversations/c1/export?format=html&include=full"},
		{"/api/memories/graph?project=p1", "/memories/graph?project=p1"},
		{"/api/orchestrator/tasks?tag=ops&tag=&tag=urgent&secret=x&status=running", ""},
	}
	for _, c := range cases {
		e.do(signedIn(t, req("GET", c.target, "")))
		if c.want == "" {
			continue
		}
		if got := e.chat.last(t).URI; got != c.want {
			t.Errorf("%s → %q, want %q", c.target, got, c.want)
		}
	}
	// The orchestrator allow-list keeps every value of a repeated param, in
	// allow-list order, and drops empties and unknowns.
	if got := e.orch.last(t).URI; got != "/tasks?status=running&tag=ops&tag=urgent" {
		t.Errorf("orchestrator tasks query %q", got)
	}
	e.do(signedIn(t, req("DELETE", "/api/connector-prefs?kind=mcp&id=a%2Fb&x=1", "", "Origin", "http://fleet.example.com")))
	if got := e.chat.last(t).URI; got != "/connector-prefs?kind=mcp&id=a%2Fb" {
		t.Errorf("connector-prefs DELETE %q", got)
	}
}

func TestBodyRules(t *testing.T) {
	e := newTestEnv(t)
	origin := []string{"Origin", "http://fleet.example.com"}
	// A no-body route sends nothing upstream even when the browser sent one.
	e.do(signedIn(t, req("POST", "/api/conversations/c1/cancel", `{"x":1}`, origin...)))
	if got := e.chat.last(t); got.Body != "" || got.Header.Get("Content-Type") != "" {
		t.Errorf("cancel forwarded a body: %q", got.Body)
	}
	// An empty DELETE /api/conversations body stays absent (legacy delete-all).
	e.do(signedIn(t, req("DELETE", "/api/conversations", "", origin...)))
	if got := e.chat.last(t); got.Body != "" || got.Header.Get("Content-Type") != "" {
		t.Errorf("empty delete: %q %q", got.Body, got.Header.Get("Content-Type"))
	}
	// summarize defaults an empty body to {}.
	e.do(signedIn(t, req("POST", "/api/conversations/c1/summarize", "", origin...)))
	if got := e.chat.last(t); got.Body != "{}" || got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("summarize: %q", got.Body)
	}
}

func TestResponseShapes(t *testing.T) {
	e := newTestEnv(t)
	origin := []string{"Origin", "http://fleet.example.com"}
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

	// 204 → 200 {"ok":true} on the routes that said so.
	res := e.do(signedIn(t, req("POST", "/api/conversations/c1/pin", `{}`, origin...)))
	if body := readBody(t, res); res.StatusCode != 200 || body != `{"ok":true}` {
		t.Errorf("pin 204: %d %q", res.StatusCode, body)
	}
	// …and a bare 204 elsewhere.
	res = e.do(signedIn(t, req("POST", "/api/push/subscribe", `{}`, origin...)))
	if res.StatusCode != 204 || readBody(t, res) != "" {
		t.Errorf("subscribe 204: %d", res.StatusCode)
	}

	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="p.json"`)
		w.Header().Set("X-Internal", "secret")
		w.Header().Set("Set-Cookie", "x=y")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "body")
	}
	// The content-type-only routes drop everything else.
	res = e.do(signedIn(t, req("GET", "/api/personas", "")))
	if res.StatusCode != http.StatusTeapot || readBody(t, res) != "body" || res.Header.Get("Content-Disposition") != "" || res.Header.Get("X-Internal") != "" || res.Header.Get("Set-Cookie") != "" {
		t.Errorf("personas: %d %v", res.StatusCode, res.Header)
	}
	// The forwardedHeaders routes keep Content-Disposition (#896).
	res = e.do(signedIn(t, req("GET", "/api/projects/p1/export", "")))
	if res.Header.Get("Content-Disposition") != `attachment; filename="p.json"` || res.Header.Get("X-Internal") != "" {
		t.Errorf("project export: %v", res.Header)
	}
	// The forced-JSON routes.
	res = e.do(signedIn(t, req("GET", "/api/conversations/c1/queue", "")))
	if res.Header.Get("Content-Type") != "application/json" {
		t.Errorf("queue content type %q", res.Header.Get("Content-Type"))
	}
	// signout forwards only the status.
	res = e.do(signedIn(t, req("POST", "/api/remote-mcp-servers/r1/signout", "", origin...)))
	if res.StatusCode != http.StatusTeapot || readBody(t, res) != "" {
		t.Errorf("signout: %d", res.StatusCode)
	}

	// A missing upstream Content-Type falls back to JSON.
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"a":1}`)) }
	res = e.do(signedIn(t, req("GET", "/api/server-config", "")))
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/") {
		t.Errorf("fallback content type %q", res.Header.Get("Content-Type"))
	}
}

func TestRevokedSessionClearsTheCookie(t *testing.T) {
	e := newTestEnv(t)
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Session-Revoked", "1")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"session_revoked"}`)
	}
	res := e.do(signedIn(t, req("GET", "/api/conversations", "")))
	if res.StatusCode != 401 || !regexp.MustCompile(`elcano_session=;.*Max-Age=0`).MatchString(setCookies(res)) {
		t.Fatalf("%d %q", res.StatusCode, setCookies(res))
	}
	// An unrelated 401 leaves it alone.
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }
	if c := setCookies(e.do(signedIn(t, req("GET", "/api/conversations", "")))); strings.Contains(c, "elcano_session=;") {
		t.Fatalf("unrelated 401 cleared the cookie: %q", c)
	}
	// The orchestrator's verdict is honoured the same way.
	e.orch.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Session-Revoked", "1")
		w.WriteHeader(http.StatusUnauthorized)
	}
	if c := setCookies(e.do(signedIn(t, req("GET", "/api/orchestrator/stats", "")))); !strings.Contains(c, "elcano_session=;") {
		t.Fatalf("orchestrator revocation ignored: %q", c)
	}
}

func TestOrchestratorCookieIdentityAndCSRFForBearer(t *testing.T) {
	e := newTestEnv(t)
	e.do(signedIn(t, req("GET", "/api/orchestrator/me", "")))
	got := e.orch.last(t)
	if got.URI != "/api/me" || got.Header.Get("X-Orchestrator-Server-Token") != testOrchToken || got.Header.Get("X-User-Email") != "alice@example.com" || got.Header.Get("X-User-Session-Epoch") != "epoch-1" {
		t.Fatalf("%q %v", got.URI, got.Header)
	}
	// A bearer client's mutating call still needs an Origin.
	res := e.do(req("POST", "/api/orchestrator/tasks", `{}`, "Authorization", "Bearer moc"))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer POST without Origin: %d", res.StatusCode)
	}
	res = e.do(req("POST", "/api/orchestrator/tasks", `{"prompt":"p"}`, "Authorization", "Bearer moc", "Origin", "http://fleet.example.com", "Content-Type", "application/json"))
	if res.StatusCode != 200 {
		t.Fatalf("bearer POST: %d", res.StatusCode)
	}
	got = e.orch.last(t)
	if got.Body != `{"prompt":"p"}` || got.Header.Get("Content-Type") != "application/json" || got.Header.Get("Origin") != "" {
		t.Fatalf("%q %v", got.Body, got.Header)
	}
}

func TestFileRoutesHardenAgentWrittenContent(t *testing.T) {
	e := newTestEnv(t)
	serve := func(ct string) {
		e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.Header().Set("Content-Length", "4")
			w.Header().Set("ETag", `"e1"`)
			_, _ = io.WriteString(w, "data")
		}
		e.orch.respond = e.chat.respond
	}
	serve("text/html; charset=utf-8")
	res := e.do(signedIn(t, req("GET", "/api/conversations/c1/workspace/out/re%20port.html", "")))
	if res.Header.Get("Content-Disposition") != "attachment; filename*=UTF-8''re%20port.html" || res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("ETag") != `"e1"` {
		t.Fatalf("workspace html: %v", res.Header)
	}
	if got := e.chat.last(t).URI; got != "/conversations/c1/workspace/out/re%20port.html" {
		t.Fatalf("upstream %q", got)
	}
	// An encoded slash stays inside its segment, as in the Next catch-all.
	e.do(signedIn(t, req("GET", "/api/conversations/c1/workspace/a%2Fb.png", "")))
	if got := e.chat.last(t).URI; got != "/conversations/c1/workspace/a%2Fb.png" {
		t.Fatalf("encoded slash: %q", got)
	}
	serve("image/png")
	res = e.do(signedIn(t, req("GET", "/api/conversations/c1/workspace/chart.png", "")))
	if res.Header.Get("Content-Disposition") != "" || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image forced to download: %v", res.Header)
	}
	// Team files: sandbox CSP, and an untyped file downloads too.
	serve("")
	res = e.do(signedIn(t, req("GET", "/api/conversations/c1/team-files/x.bin", "")))
	if res.Header.Get("Content-Security-Policy") != "sandbox" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("team file: %v", res.Header)
	}
	res = e.do(signedIn(t, req("HEAD", "/api/conversations/c1/team-files/x.bin", "")))
	if got := e.chat.last(t); got.Method != "HEAD" || res.StatusCode != 200 {
		t.Fatalf("HEAD forwarded as %s (%d)", got.Method, res.StatusCode)
	}
	serve("image/svg+xml")
	res = e.do(signedIn(t, req("GET", "/api/orchestrator/tasks/t1/workspace/chart.svg", "")))
	if !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("orchestrator svg: %v", res.Header)
	}
	// A refusal is forwarded as its plain body.
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusNotFound) }
	res = e.do(signedIn(t, req("GET", "/api/conversations/c1/workspace/x", "")))
	if res.StatusCode != 404 || res.Header.Get("Content-Type") != "text/plain;charset=UTF-8" {
		t.Fatalf("refusal: %d %v", res.StatusCode, res.Header)
	}
}

func TestSSEStreamsAndFlushesFrameByFrame(t *testing.T) {
	e := newTestEnv(t)
	release := make(chan struct{})
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Fleet-Conversation-Id", "c-9")
		w.Header().Set("X-Fleet-Heartbeat-Interval-Ms", "15000")
		w.Header().Set("X-Other", "dropped")
		_, _ = io.WriteString(w, "event: text.delta\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: turn.completed\ndata: {}\n\n")
	}
	srv := httptest.NewServer(e.g)
	defer srv.Close()
	r, _ := http.NewRequest("POST", srv.URL+"/api/chat", strings.NewReader(`{"message":"hi"}`))
	r.Header.Set("Origin", srv.URL)
	r.AddCookie(&http.Cookie{Name: websession.CookieName, Value: mintHMAC(t, "a@x.com")})
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || res.Header.Get("X-Fleet-Conversation-Id") != "c-9" ||
		res.Header.Get("X-Fleet-Heartbeat-Interval-Ms") != "15000" || res.Header.Get("X-Other") != "" || res.Header.Get("Cache-Control") != "no-cache, no-transform" {
		t.Fatalf("headers %v", res.Header)
	}
	// The first frame arrives while the handler is still blocked: no buffering.
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := res.Body.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, "text.delta") {
			t.Fatalf("first read %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first SSE frame was buffered")
	}
	close(release)
}

func TestUploadStreamsUntruncated(t *testing.T) {
	e := newTestEnv(t)
	var n int64
	e.chat.respond = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}
	// 25 MiB — past the 10 MB the Next tier truncated at by default.
	size := int64(25 << 20)
	body := strings.Repeat("a", int(size))
	r := signedIn(t, req("POST", "/api/attachments", body, "Origin", "http://fleet.example.com", "Content-Type", "multipart/form-data; boundary=x"))
	res := e.do(r)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	got := e.chat.last(t)
	n = int64(len(got.Body))
	if n != size || got.Header.Get("Content-Type") != "multipart/form-data; boundary=x" {
		t.Fatalf("upstream got %d bytes (%q)", n, got.Header.Get("Content-Type"))
	}
	// Non-multipart is refused before the backend.
	before := e.chat.count()
	res = e.do(signedIn(t, req("POST", "/api/attachments", "x", "Origin", "http://fleet.example.com", "Content-Type", "text/plain")))
	if res.StatusCode != 400 || e.chat.count() != before {
		t.Fatalf("non-multipart: %d", res.StatusCode)
	}
}

func TestNoBackendIs502(t *testing.T) {
	g, err := New(Config{SessionSecret: testSecret}, Options{Bundle: testBundle()})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, signedIn(t, req("GET", "/api/personas", "")))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "chat-server unreachable") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestProjectFilesFocusValidation(t *testing.T) {
	e := newTestEnv(t)
	res := e.do(signedIn(t, req("GET", "/api/projects/p1/files?focus=../x", "")))
	if res.StatusCode != 400 {
		t.Fatalf("%d", res.StatusCode)
	}
	e.do(signedIn(t, req("GET", "/api/projects/p1/files?focus=f_1", "")))
	if got := e.chat.last(t).URI; got != "/projects/p1/files?focus=f_1" {
		t.Fatalf("%q", got)
	}
}

func TestTeamViewForwardsIfNoneMatch(t *testing.T) {
	e := newTestEnv(t)
	e.do(signedIn(t, req("GET", "/api/conversations/c1/team-view", "", "If-None-Match", `"v1"`)))
	if got := e.chat.last(t).Header.Get("If-None-Match"); got != `"v1"` {
		t.Fatalf("%q", got)
	}
}

func TestStreamReattachForwardsLastEventIDAndQuery(t *testing.T) {
	e := newTestEnv(t)
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	res := e.do(signedIn(t, req("GET", "/api/conversations/c1/stream?turn_id=t9", "", "Last-Event-ID", "42")))
	got := e.chat.last(t)
	if got.URI != "/conversations/c1/stream?turn_id=t9" || got.Header.Get("Last-Event-ID") != "42" || res.StatusCode != 204 {
		t.Fatalf("%q %v %d", got.URI, got.Header, res.StatusCode)
	}
}
