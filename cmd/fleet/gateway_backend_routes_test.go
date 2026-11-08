package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
	"github.com/ElcanoTek/fleet/internal/webui"
)

// TestGatewayRowsResolveOnBackends holds every table-driven browser gateway
// row (internal/webui chatRoutes) to a backend route that exists.
//
// The gateway forwards /api/orchestrator/* and /api/* to the in-process
// orchestrator and chat handlers by path, and the web tests mock the API, so
// a row whose target no backend serves is invisible until a person presses
// the button and gets a 404. That is how the Settings → Connections
// credential-account add/delete shipped dead: the rows were ported from the
// retired Next.js tier (ADR-0091), but the orchestrator never registered
// /mcp-servers/{server}/accounts.
//
//   - The Operations Center has no rows any more: /api/orchestrator/* is the
//     orchestrator router itself (ADR-0110). Its query policy is held to the
//     real router by TestOrchestratorQueryPolicyNamesRealRoutes.
//   - Chat rows are probed through the REAL chat-server handler without its
//     shared token: a served path stops at the auth gate (403); an unserved
//     one is the ServeMux's 404. The chat server registers prefix patterns
//     (/conversations/, /projects/, ...) and dispatches sub-paths in its
//     handlers, so for those this proves the prefix is served, not the
//     sub-route.
func TestGatewayRowsResolveOnBackends(t *testing.T) {
	t.Parallel()

	h := handlers.New(handlers.Config{}, nil, nil)
	orch, ok := buildOrchestratorMux(h, handlers.NewNotesHandlers(nil, h), reloadConfigHandler(nil), mcpReloadHandler(nil), nil).(chi.Routes)
	if !ok {
		t.Fatal("orchestrator router is not chi.Routes")
	}
	chat := httpapi.New(&config.Config{SharedToken: "drift-test-token"}, nil, nil).Routes()

	// Negative controls: both probes must be able to say "not served", or the
	// loop below proves nothing.
	if orch.Match(chi.NewRouteContext(), http.MethodPost, "/mcp-servers/x1/no-such-route") {
		t.Fatal("orchestrator Match accepted an unregistered path")
	}
	ctl := httptest.NewRecorder()
	chat.ServeHTTP(ctl, httptest.NewRequest(http.MethodGet, "/no-such-route", nil))
	if ctl.Code != http.StatusNotFound {
		t.Fatalf("chat-server answered %d for an unregistered path; the probe cannot tell served from unserved", ctl.Code)
	}

	rows := webui.BackendRoutes()
	var nChat int
	for _, rt := range rows {
		switch rt.Backend {
		case webui.BackendChat:
			nChat++
			w := httptest.NewRecorder()
			chat.ServeHTTP(w, httptest.NewRequest(rt.Method, rt.Target, nil))
			if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
				t.Errorf("gateway row %q forwards to chat-server %s %s, which answers %d", rt.Pattern, rt.Method, rt.Target, w.Code)
			}
		default:
			t.Errorf("gateway row %q names unknown backend %q", rt.Pattern, rt.Backend)
		}
	}
	// A table that silently empties would make this test vacuous.
	if nChat < 100 {
		t.Fatalf("walked %d chat rows; the route table looks truncated", nChat)
	}
}

// TestOrchestratorQueryPolicyNamesRealRoutes: every route the browser alias
// filters the query of is a route the real orchestrator router registers, so
// renaming one cannot silently start forwarding the browser's whole query.
func TestOrchestratorQueryPolicyNamesRealRoutes(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, r := range realOrchestratorRoutes(t) {
		registered[r.Key()] = true
	}
	keys := webui.OrchestratorQueryPolicyRoutes()
	if len(keys) < 40 {
		t.Fatalf("the query policy lists %d routes; it looks truncated", len(keys))
	}
	for _, k := range keys {
		if !registered[k] {
			t.Errorf("the browser alias's query policy names %q, which the orchestrator router does not register", k)
		}
	}
}
