package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/principal"
	"github.com/ElcanoTek/fleet/internal/websession"
	"github.com/ElcanoTek/fleet/internal/webui"
)

func TestWebAddr(t *testing.T) {
	// The padded value checks that surrounding whitespace is trimmed.
	for _, c := range []struct{ v, want string }{{"", defaultWebAddr}, {"\t0.0.0.0:3000\n", "0.0.0.0:3000"}} {
		t.Setenv("FLEET_WEB_ADDR", c.v)
		if got, on := webAddr(); !on || got != c.want {
			t.Errorf("%q → %q %v", c.v, got, on)
		}
	}
	for _, v := range []string{"off", "OFF", "none", "disabled", "0"} {
		t.Setenv("FLEET_WEB_ADDR", v)
		if _, on := webAddr(); on {
			t.Errorf("%q left the web tier on", v)
		}
	}
}

func TestPrintWebBundleReportsTheEmbeddedBundle(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	code := printWebBundle(&out)
	if id := webui.EmbeddedBuildID(); id == "" {
		if code != 1 || !strings.Contains(out.String(), "missing") {
			t.Fatalf("%d %q", code, out.String())
		}
	} else if code != 0 || strings.TrimSpace(out.String()) != "web bundle: "+id {
		t.Fatalf("%d %q", code, out.String())
	}
}

// The gateway's in-process calls carry their caller as a typed principal in
// the request context and no shared secret or identity header (ADR-0108): a
// leftover ORCHESTRATOR_SERVER_TOKEN (the retired Next tier's env; an old
// bootstrap even wrote ADMIN_API_KEY there) cannot reach a backend at all.
func TestWebServerCallsBothBackendsWithThePrincipal(t *testing.T) {
	const secret = "web-test-session-secret-0123456789"
	t.Setenv("APP_SESSION_SECRET", secret)
	t.Setenv("ORCHESTRATOR_SERVER_TOKEN", "stale-admin-key")
	t.Setenv("FLEET_WEB_ADDR", "")
	type seen struct {
		who     principal.Principal
		headers http.Header
	}
	var gotOrch, gotChat seen
	orch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrch.who, _ = principal.From(r.Context())
		gotOrch.headers = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	})
	chat := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotChat.who, _ = principal.From(r.Context())
		gotChat.headers = r.Header.Clone()
		_, _ = io.WriteString(w, `{"epoch":"e1"}`)
	})
	srv, err := buildWebServer(&config.Config{SharedToken: "the-shared-token"}, backendHandlers{chat: chat, orch: orch})
	if err != nil || srv == nil {
		t.Fatalf("buildWebServer: %v", err)
	}
	// Slowloris-safe header reads and bounded idle keep-alives, but no
	// whole-request read or write deadline: those would cut off uploads and
	// SSE turns.
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 || srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("timeouts: header %v idle %v read %v write %v", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.ReadTimeout, srv.WriteTimeout)
	}
	tok, err := websession.NewHMAC(secret).Mint("alice@example.com", "e1")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/orchestrator/stats", "/api/personas"} {
		r := httptest.NewRequest("GET", target, nil)
		r.AddCookie(&http.Cookie{Name: websession.CookieName, Value: tok})
		srv.Handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	for name, got := range map[string]seen{"orchestrator": gotOrch, "chat": gotChat} {
		if got.who.Email != "alice@example.com" || got.who.AuthMethod != principal.MethodSession || got.who.Session.Epoch != "e1" {
			t.Errorf("%s principal %+v", name, got.who)
		}
		for _, h := range []string{"X-Chat-Server-Token", "X-Orchestrator-Server-Token", "X-User-Email"} {
			if got.headers.Get(h) != "" {
				t.Errorf("%s got %s", name, h)
			}
		}
	}
}

// config.OIDCConfigured decides whether Fleet passwords are off; webui's own
// parse decides whether SSO is offered. They must agree on every input, or a
// box could refuse passwords while offering no SSO in their place.
func TestPasswordSwitchAgreesWithTheSSOButton(t *testing.T) {
	cases := []map[string]string{
		{},
		{"FLEET_OIDC_ISSUER": "https://auth.example.com", "FLEET_OIDC_CLIENT_ID": "fleet", "FLEET_OIDC_CLIENT_SECRET": "s"},
		{"FLEET_OIDC_ISSUER": "https://auth.example.com/", "FLEET_OIDC_CLIENT_ID": " fleet ", "FLEET_OIDC_CLIENT_SECRET": "s"},
		{"FLEET_OIDC_ISSUER": "///", "FLEET_OIDC_CLIENT_ID": "fleet", "FLEET_OIDC_CLIENT_SECRET": "s"},
		{"FLEET_OIDC_ISSUER": "https://auth.example.com", "FLEET_OIDC_CLIENT_ID": "", "FLEET_OIDC_CLIENT_SECRET": "s"},
		{"FLEET_OIDC_ISSUER": "https://auth.example.com", "FLEET_OIDC_CLIENT_ID": "fleet", "FLEET_OIDC_CLIENT_SECRET": " \t"},
		{"FLEET_OIDC_ISSUER": "  ", "FLEET_OIDC_CLIENT_ID": "fleet", "FLEET_OIDC_CLIENT_SECRET": "s"},
	}
	for _, env := range cases {
		for _, k := range []string{"FLEET_OIDC_ISSUER", "FLEET_OIDC_CLIENT_ID", "FLEET_OIDC_CLIENT_SECRET"} {
			t.Setenv(k, env[k])
		}
		if got, offered := config.OIDCConfigured(), webui.ConfigFromEnv().OIDC != nil; got != offered {
			t.Errorf("%v: config says configured=%v, webui offers SSO=%v", env, got, offered)
		}
	}
}

// buildWebServer hands the gateway the decision config.Load made, so the
// gateway refuses the password form exactly when the backends refuse
// passwords.
func TestWebServerTakesThePasswordSwitchFromConfig(t *testing.T) {
	t.Setenv("APP_SESSION_SECRET", "web-test-session-secret-0123456789")
	t.Setenv("FLEET_WEB_ADDR", "")
	for _, off := range []bool{false, true} {
		var verified bool
		chat := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			verified = verified || r.URL.Path == "/auth/verify"
			_, _ = io.WriteString(w, `{"ok":false}`)
		})
		srv, err := buildWebServer(&config.Config{SharedToken: "tok", PasswordLoginDisabled: off}, backendHandlers{chat: chat, orch: http.NotFoundHandler()})
		if err != nil || srv == nil {
			t.Fatalf("buildWebServer: %v", err)
		}
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader("email=a%40x.com&password=p"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://example.com")
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, r)
		refused := strings.HasSuffix(w.Header().Get("Location"), "/login?e=password_off")
		if refused != off || verified == off {
			t.Errorf("PasswordLoginDisabled=%v: location %q, password checked %v", off, w.Header().Get("Location"), verified)
		}
	}
}

// serve builds the Operations Center's handlers.Config inline, so no test can
// call it. This pins, from the source, that the literal hands the scheduler
// the accounts decision config.Load made: without it /v1/users would still
// create Ops identities on a Fleet whose other planes leave that to Auth. (The
// Operations Center has no password login of its own since #2049 M4, so there
// is no password switch to hand it.)
func TestServeHandsTheSchedulerThePasswordSwitch(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"AccountsManagedByAuth"} {
		literals, wired := countConfigWiring(t, f, field)
		if literals == 0 || wired != literals {
			t.Fatalf("main.go builds %d handlers.Config literal(s), %d set %s: cfg.%s", literals, wired, field, field)
		}
	}
}

// countConfigWiring counts the handlers.Config literals in f and those that
// set field to cfg.<field>.
func countConfigWiring(t *testing.T, f *ast.File, field string) (literals, wired int) {
	t.Helper()
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Config" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "handlers" {
			return true
		}
		literals++
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != field {
				continue
			}
			if v, ok := kv.Value.(*ast.SelectorExpr); ok && v.Sel.Name == field {
				if x, ok := v.X.(*ast.Ident); ok && x.Name == "cfg" {
					wired++
				}
			}
		}
		return true
	})
	return literals, wired
}

// buildWebServer also hands the gateway config.Load's account-management
// decision, so the pages go view-only exactly when chat-server refuses the
// writes. (The page bootstrap this drives needs a web bundle, which a Go-only
// build lacks; internal/webui renders it against a test bundle.)
func TestWebServerTakesAccountManagementFromConfig(t *testing.T) {
	t.Parallel()

	for _, managed := range []bool{false, true} {
		if got := gatewayConfig(&config.Config{AccountsManagedByAuth: managed}).AccountsManagedByAuth; got != managed {
			t.Errorf("AccountsManagedByAuth=%v reached the gateway as %v", managed, got)
		}
	}
}
