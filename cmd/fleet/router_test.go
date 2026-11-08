package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/apispec"
	"github.com/ElcanoTek/fleet/internal/apispec/orchestrator"
	"github.com/ElcanoTek/fleet/internal/apiversion"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/webui"
)

// realOrchestratorRouter is buildOrchestratorMux with no stores behind it:
// enough to route, walk and answer the requests that need no database.
func realOrchestratorRouter(t *testing.T) *chi.Mux {
	t.Helper()
	h := handlers.New(handlers.Config{}, nil, nil)
	orch, ok := buildOrchestratorMux(h, handlers.NewNotesHandlers(nil, h), reloadConfigHandler(nil), mcpReloadHandler(nil), nil).(*chi.Mux)
	if !ok {
		t.Fatal("orchestrator router is not a *chi.Mux")
	}
	return orch
}

// contractRouter is the public tree the contract is generated from: the real
// orchestrator router at its mounts, the other families stubbed.
func contractRouter(t *testing.T) publicRouter {
	t.Helper()
	orch := realOrchestratorRouter(t)
	return buildPublicRouter(httpSurface{
		gateway: http.NotFoundHandler(), orch: orch,
		orchListener: apiversion.Router(orch), chatListener: http.NotFoundHandler(),
	})
}

func routesUnder(t *testing.T, r chi.Routes, prefix string) []string {
	t.Helper()
	walked, err := apispec.WalkChi(r)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, rt := range walked {
		if p, ok := strings.CutPrefix(rt.Pattern, prefix); ok && strings.HasPrefix(p, "/") {
			out = append(out, rt.Method+" "+p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestBrowserAliasMountsTheV1RouteSet: /api/orchestrator is an alias of /v1,
// not a second table (ADR-0109 §2): the public tree registers exactly the same
// route set under both, and both equal the orchestrator router's own.
func TestBrowserAliasMountsTheV1RouteSet(t *testing.T) {
	t.Parallel()
	root := contractRouter(t)
	v1 := routesUnder(t, root, orchestrator.V1Prefix)
	alias := routesUnder(t, root, webui.OrchestratorPrefix)
	own := routesUnder(t, realOrchestratorRouter(t), "")
	if len(v1) < 90 {
		t.Fatalf("only %d routes under /v1; the walk did not reach the orchestrator router", len(v1))
	}
	if !slices.Equal(v1, alias) {
		t.Errorf("/v1 and %s register different routes:\n /v1   %v\n alias %v", webui.OrchestratorPrefix, v1, alias)
	}
	if !slices.Equal(v1, own) {
		t.Errorf("/v1 does not register the orchestrator router's routes:\n /v1 %v\n own %v", v1, own)
	}
}

// TestPublicRouterFamilies: each path reaches the family that owns it, for
// every method, and the prefixes match whole segments.
func TestPublicRouterFamilies(t *testing.T) {
	t.Parallel()
	marker := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if chi.RouteContext(r.Context()) != nil {
				t.Errorf("%s %s reached %s with the parent tree's routing state", r.Method, r.URL.Path, name)
			}
			_, _ = io.WriteString(w, name)
		})
	}
	root := buildPublicRouter(httpSurface{
		gateway: marker("webui"), orch: realOrchestratorRouter(t),
		orchListener: marker("orchestrator"), chatListener: marker("chat"),
	})
	cases := []struct{ method, path, want string }{
		{"GET", "/v1", "orchestrator"},
		{"GET", "/v1/", "orchestrator"},
		{"GET", "/v1/tasks", "orchestrator"},
		{"POST", "/v1/tasks/x/wake", "orchestrator"},
		{"GET", "/v1/no-such-route", "orchestrator"},
		{"GET", "/api-info", "orchestrator"},
		{"GET", "/.well-known/agent-card.json", "orchestrator"},
		{"POST", "/a2a", "orchestrator"},
		{"POST", "/a2a/", "orchestrator"},
		{"POST", "/triggers/s1", "orchestrator"},
		{"POST", "/triggers/email/s1", "orchestrator"},
		{"POST", "/webhooks/github/x", "chat"},
		{"GET", "/api/orchestrator", "webui"},
		{"GET", "/api/orchestrator/tasks", "webui"},
		{"GET", "/api/orchestratorx", "webui"},
		{"GET", "/v1x", "webui"},
		{"GET", "/", "webui"},
		{"GET", "/chat", "webui"},
		{"GET", "/api/personas", "webui"},
		{"GET", "/webhooks", "webui"},
		{"GET", "/healthz", "webui"},
		// Path first, for every method: the family that owns the path
		// answers it, whatever the method (chi alone would answer a method
		// it does not know with the root's 405 before looking at the path).
		{"PROPFIND", "/", "webui"},
		{"PROPFIND", "/v1/tasks", "orchestrator"},
		{"PROPFIND", "/a2a", "orchestrator"},
		{"PROPFIND", "/webhooks/x", "chat"},
		{"PROPFIND", "/api/orchestrator/tasks", "webui"},
		{"OPTIONS", "/v1/tasks", "orchestrator"},
		{"TRACE", "/triggers/s1", "orchestrator"},
		{"BREW", "/webhooks/x", "chat"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if got := rec.Body.String(); got != c.want {
			t.Errorf("%s %s → %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// TestPublicRouterAnswersV1LikeTheOrchestratorListener: the public API on the
// public tree is the orchestrator listener's handler, byte for byte — the
// requests a client like manifest makes (X-API-Key, no session) get the same
// status, headers and body from both.
func TestPublicRouterAnswersV1LikeTheOrchestratorListener(t *testing.T) {
	t.Parallel()
	orch := realOrchestratorRouter(t)
	listener := apiversion.Router(orch)
	root := buildPublicRouter(httpSurface{gateway: http.NotFoundHandler(), orch: orch, orchListener: listener, chatListener: http.NotFoundHandler()})
	reqs := []func() *http.Request{
		func() *http.Request { return httptest.NewRequest("GET", "/v1/api-info", nil) },
		func() *http.Request { return httptest.NewRequest("GET", "/api-info", nil) },
		func() *http.Request { return httptest.NewRequest("GET", "/v1/tasks", nil) },
		func() *http.Request {
			return httptest.NewRequest("POST", "/v1/tasks", strings.NewReader(`{"prompt":"x"}`))
		},
		func() *http.Request { return httptest.NewRequest("GET", "/v1/no-such-route", nil) },
		func() *http.Request { return httptest.NewRequest("DELETE", "/v1/api-info", nil) },
		func() *http.Request { return httptest.NewRequest("GET", "/.well-known/agent-card.json", nil) },
		func() *http.Request { return httptest.NewRequest("POST", "/v1/a2a", strings.NewReader(`{}`)) },
	}
	for _, mk := range reqs {
		a, b := httptest.NewRecorder(), httptest.NewRecorder()
		ra, rb := mk(), mk()
		root.ServeHTTP(a, ra)
		listener.ServeHTTP(b, rb)
		strip := func(h http.Header) http.Header { h = h.Clone(); h.Del("X-Request-Id"); return h }
		if a.Code != b.Code || !reflect.DeepEqual(strip(a.Header()), strip(b.Header())) || a.Body.String() != b.Body.String() {
			t.Errorf("%s %s: public tree %d %v %q; orchestrator listener %d %v %q",
				ra.Method, ra.URL.Path, a.Code, a.Header(), a.Body.String(), b.Code, b.Header(), b.Body.String())
		}
	}
}

// TestPublicRouterStreamsUnbuffered: the tree neither buffers nor wraps a
// response, so a family's flush (the SSE streams) reaches the connection.
func TestPublicRouterStreamsUnbuffered(t *testing.T) {
	t.Parallel()
	flusher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: 1\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
	})
	root := buildPublicRouter(httpSurface{gateway: flusher, orch: chi.NewRouter(), orchListener: flusher, chatListener: flusher})
	for _, p := range []string{"/v1/tasks/x/stream", "/webhooks/x", "/api/conversations/x/stream"} {
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if !rec.Flushed {
			t.Errorf("%s: the flush did not reach the connection", p)
		}
	}
	if n := len(root.Middlewares()); n != 0 {
		t.Errorf("the public tree has %d middlewares of its own; each family's chain must be its whole chain", n)
	}
}

func funcName(f any) string {
	name := runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	name = strings.TrimSuffix(name, "-fm")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// TestOrchestratorMiddlewareChainOrder pins the orchestrator family's
// router-level chain. It is the whole chain on every mount (/v1, the browser
// alias, the orchestrator listener): the public tree adds none
// (TestPublicRouterStreamsUnbuffered) and each mount routes afresh.
func TestOrchestratorMiddlewareChainOrder(t *testing.T) {
	t.Parallel()
	mws := realOrchestratorRouter(t).Middlewares()
	got := make([]string, 0, len(mws))
	for _, mw := range mws {
		got = append(got, funcName(mw))
	}
	want := []string{
		"httpmw.TrustedProxies.Middleware.func1", // client IP (FLEET_TRUSTED_PROXIES)
		"httpmw.Recoverer",
		"otelsetup.Middleware", // tracing span + X-Request-Id
		"fleet.orchestratorMetricsMiddleware",
		"handlers.(*Handlers).SecurityHeadersMiddleware",
		"handlers.(*Handlers).BodySizeLimitMiddleware",
		"handlers.(*Handlers).CSRFMiddleware",
	}
	if !slices.Equal(got, want) {
		t.Errorf("orchestrator chain:\n got  %v\n want %v", got, want)
	}
}
