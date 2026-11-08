package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/apispec"
	"github.com/ElcanoTek/fleet/internal/apiversion"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/websession"
	"github.com/ElcanoTek/fleet/internal/webui"
)

const paritySecret = "web-test-session-secret-0123456789"

// pageCSP is the gateway's page policy for this test's config; the frozen
// table abbreviates it.
const pageCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https:; font-src 'self' data:; connect-src 'self'; media-src 'self' data: blob:; frame-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; worker-src 'self' blob:"

// recorder is a backend double that records what reached it: the request as
// the backend saw it and, for a chi router, the route pattern that served it.
type recorder struct {
	mu      sync.Mutex
	uri     string
	body    string
	ct      string
	pattern string
	called  bool
	// silent answers with no body (the method matrix was captured that way).
	silent bool
}

func (c *recorder) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uri, c.body, c.ct, c.pattern, c.called = "", "", "", "", false
}

func (c *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.called = true
	c.uri = r.Method + " " + r.RequestURI + " host=" + r.Host + " ra=" + r.RemoteAddr
	c.body = string(b)
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c.ct = r.Header.Get("Content-Type") + "|" + strings.Join(keys, ",")
	if rc := chi.RouteContext(r.Context()); rc != nil {
		c.pattern = rc.RoutePattern()
	}
	c.mu.Unlock()
	w.Header().Set("X-Secret", "1")
	w.Header().Set("Cache-Control", "max-age=5")
	w.WriteHeader(http.StatusCreated)
	if !c.silent {
		_, _ = io.WriteString(w, "{}")
	}
}

// realOrchestratorRoutes walks the real orchestrator router.
func realOrchestratorRoutes(t *testing.T) []apispec.Route {
	t.Helper()
	h := handlers.New(handlers.Config{}, nil, nil)
	orch, ok := buildOrchestratorMux(h, reloadConfigHandler(nil), mcpReloadHandler(nil), nil).(chi.Routes)
	if !ok {
		t.Fatal("orchestrator router is not chi.Routes")
	}
	routes, err := apispec.WalkChi(orch)
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

// mirrorRouter is a chi router with exactly the given routes, each answered
// by rec: the real router's routing without its handlers' dependencies.
func mirrorRouter(routes []apispec.Route, rec http.Handler) *chi.Mux {
	m := chi.NewRouter()
	for _, r := range routes {
		m.Method(r.Method, r.Pattern, rec)
	}
	return m
}

type paritySurface struct {
	root       publicRouter
	chat, orch *recorder
	orchRouter *chi.Mux
}

func newParitySurface(t *testing.T) paritySurface {
	t.Helper()
	return newParitySurfaceWith(t, false)
}

func newParitySurfaceWith(t *testing.T, silent bool) paritySurface {
	t.Helper()
	t.Setenv("APP_SESSION_SECRET", paritySecret)
	t.Setenv("FLEET_WEB_ADDR", "")
	chat, orch := &recorder{silent: silent}, &recorder{silent: silent}
	orchRouter := mirrorRouter(realOrchestratorRoutes(t), orch)
	srv, err := buildWebServer(&config.Config{}, backendHandlers{
		chat: chat, orch: orchRouter,
		chatListener: apiversion.Router(chat), orchListener: apiversion.Router(orchRouter),
	})
	if err != nil {
		t.Fatal(err)
	}
	root, ok := srv.Handler.(publicRouter)
	if !ok {
		t.Fatalf("the public listener's handler is %T, not the one chi tree", srv.Handler)
	}
	return paritySurface{root: root, chat: chat, orch: orch, orchRouter: orchRouter}
}

func samplePath(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") {
			segs[i] = "x1"
		}
	}
	return strings.Join(segs, "/")
}

func headerLines(h http.Header) string {
	var out []string
	for n, v := range h {
		if n == "Set-Cookie" || n == webui.BuildIDHeader {
			continue
		}
		out = append(out, n+"="+strings.ReplaceAll(strings.Join(v, ","), pageCSP, "<page-csp>"))
	}
	sort.Strings(out)
	return strings.Join(out, ";")
}

// TestOneSurfaceParity replays the browser route table the gateway answered
// before the one-surface router (testdata/http-surface-before.txt, captured
// from the pre-change tree) through the new public tree and requires, row for
// row: the same family and upstream route, the same answer without a session
// (the auth requirement), and with a session the same upstream request (path,
// query, headers, body, peer) and the same browser-facing status and headers.
func TestOneSurfaceParity(t *testing.T) {
	s := newParitySurface(t)
	tok, err := websession.NewHMAC(paritySecret).Mint("alice@example.com", "e1")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/http-surface-before.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var v1Before []string
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		family, method, pattern := fields[0], fields[1], fields[2]
		if family == "v1" {
			v1Before = append(v1Before, method+" "+pattern)
			continue
		}
		n++
		sample := samplePath(pattern)

		unauth := httptest.NewRequest(method, sample, nil)
		unauth.Header.Set("Origin", "http://example.com")
		ur := httptest.NewRecorder()
		s.root.ServeHTTP(ur, unauth)

		s.chat.reset()
		s.orch.reset()
		signed := httptest.NewRequest(method, sample+"?a=1&limit=5&status=x&days=3&from=f&to=t&format=csv&group_by=g&offset=2&tag=t1&tag=&q=s&deep=1&scope=team&kind=k&id=i&focus=ab", strings.NewReader(`{"x":1}`))
		signed.Header.Set("Origin", "http://example.com")
		signed.Header.Set("Content-Type", "application/json")
		signed.Header.Set("X-Forwarded-For", "9.9.9.9")
		signed.AddCookie(&http.Cookie{Name: websession.CookieName, Value: tok})
		sr := httptest.NewRecorder()
		s.root.ServeHTTP(sr, signed)

		var up *recorder
		var upstream string
		switch family {
		case "chat":
			up = s.chat
			if s.orch.called {
				t.Errorf("%s %s reached the orchestrator", method, pattern)
			}
			target, _, _ := strings.Cut(strings.Fields(up.uri + "  ")[1], "?")
			upstream = target
		case "orchestrator":
			up = s.orch
			if s.chat.called {
				t.Errorf("%s %s reached chat", method, pattern)
			}
			upstream = up.pattern
		default:
			t.Fatalf("unknown family %q", family)
		}
		upMethod := ""
		if up.uri != "" {
			upMethod = strings.Fields(up.uri)[0]
		}
		got := fmt.Sprintf("%s %s %s -> %s %s unauth=%d signed=%d up=[%s] body=%q ct=%q resp=[%s]",
			family, method, pattern, upMethod, upstream, ur.Code, sr.Code, up.uri, up.body, up.ct, headerLines(sr.Header()))
		if got != line {
			t.Errorf("row changed:\n before %s\n after  %s", line, got)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 165 || len(v1Before) < 65 {
		t.Fatalf("replayed %d browser rows and %d /v1 routes; the frozen table looks truncated", n, len(v1Before))
	}

	// Every /v1 route the orchestrator listener answered is still a /v1 route
	// of the orchestrator family on the public tree (the alias test checks
	// the routers are the same; this pins the set against the past).
	walked, err := apispec.WalkChi(s.root)
	if err != nil {
		t.Fatal(err)
	}
	var v1After []string
	for _, r := range walked {
		if strings.HasPrefix(r.Pattern, "/v1/") {
			v1After = append(v1After, "v1 "+r.Method+" "+r.Pattern)
		}
	}
	for i, k := range v1Before {
		v1Before[i] = "v1 " + k
	}
	slices.Sort(v1Before)
	slices.Sort(v1After)
	v1After = slices.Compact(v1After)
	if !slices.Equal(v1Before, v1After) {
		t.Errorf("/v1 route set changed:\n before %v\n after  %v", v1Before, v1After)
	}
}

// parityMethods is every method the matrix tests try on every path: the
// standard ones and one chi does not know.
var parityMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PROPFIND"}

// TestOneSurfaceMethodParity is the method matrix. Method handling on the
// tree is path-first and family-first, so no method on any path may reach
// another family's 404, 405 or redirect:
//
//   - every browser path the gateway answered (the rows of
//     http-surface-before.txt plus the gateway's own Operations Center paths
//     and a few unknown ones), tried with every method, with and without a
//     session, answers the status, Allow and Location it answered before the
//     change (testdata/http-surface-methods-before.txt, captured from the
//     pre-change tree);
//   - every /v1 and unversioned-forever path, tried with every method,
//     answers the status and Allow the orchestrator listener answers.
//
// The one deliberate difference is newAliasPaths: router paths the browser
// had no row for. They answered the gateway's 404 and now answer as the
// router's alias (ADR-0110); they are checked against that instead.
var newAliasPaths = map[string]bool{"/api/orchestrator/api/config": true, "/api/orchestrator/keys": true}

func TestOneSurfaceMethodParity(t *testing.T) {
	s := newParitySurfaceWith(t, true)
	tok, err := websession.NewHMAC(paritySecret).Mint("alice@example.com", "e1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/http-surface-methods-before.txt")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		fields := strings.Fields(line)
		pattern, m := fields[0], fields[1]
		sample := samplePath(pattern)
		u := httptest.NewRequest(m, sample, nil)
		u.Header.Set("Origin", "http://example.com")
		ur := httptest.NewRecorder()
		s.root.ServeHTTP(ur, u)
		sr := httptest.NewRequest(m, sample, strings.NewReader("{}"))
		sr.Header.Set("Origin", "http://example.com")
		sr.Header.Set("Content-Type", "application/json")
		sr.AddCookie(&http.Cookie{Name: websession.CookieName, Value: tok})
		srec := httptest.NewRecorder()
		s.root.ServeHTTP(srec, sr)
		got := fmt.Sprintf("%s %s unauth=%d allow=%q loc=%q signed=%d allow=%q loc=%q", pattern, m,
			ur.Code, ur.Header().Get("Allow"), ur.Header().Get("Location"), srec.Code, srec.Header().Get("Allow"), srec.Header().Get("Location"))
		if newAliasPaths[pattern] {
			// Now a router path: never the gateway's 404, the gate still first,
			// a registered method served, any other a 405 naming the router's.
			if ur.Code != http.StatusUnauthorized || srec.Code == http.StatusNotFound ||
				(srec.Code == http.StatusMethodNotAllowed) == (srec.Code == http.StatusCreated) {
				t.Errorf("new alias path: %s", got)
			}
			continue
		}
		if got != line {
			t.Errorf("method row changed:\n before %s\n after  %s", line, got)
		}
	}
	if n < 1000 {
		t.Fatalf("replayed %d method rows; the frozen matrix looks truncated", n)
	}

	// /v1 and the unversioned-forever paths against the orchestrator
	// listener, with the real router behind both.
	orch := realOrchestratorRouter(t)
	listener := apiversion.Router(orch)
	root := buildPublicRouter(httpSurface{gateway: http.NotFoundHandler(), orch: orch, orchListener: listener, chatListener: http.NotFoundHandler()})
	paths := map[string]bool{"/api-info": true, "/.well-known/agent-card.json": true, "/a2a": true, "/a2a/": true, "/triggers/s1": true, "/triggers/email/s1": true, "/v1": true, "/v1/": true, "/v1/no-such-route": true}
	for _, r := range realOrchestratorRoutes(t) {
		paths["/v1"+samplePath(strings.ReplaceAll(r.Pattern, "*", "x1"))] = true
	}
	for p := range paths {
		for _, m := range parityMethods {
			a, b := httptest.NewRecorder(), httptest.NewRecorder()
			root.ServeHTTP(a, httptest.NewRequest(m, p, nil))
			listener.ServeHTTP(b, httptest.NewRequest(m, p, nil))
			// chi sends one Allow header per method, in map order.
			aa, ba := slices.Sorted(slices.Values(a.Header().Values("Allow"))), slices.Sorted(slices.Values(b.Header().Values("Allow")))
			if a.Code != b.Code || !slices.Equal(aa, ba) {
				t.Errorf("%s %s: public tree %d Allow=%q, orchestrator listener %d Allow=%q", m, p, a.Code, aa, b.Code, ba)
			}
		}
	}
}
