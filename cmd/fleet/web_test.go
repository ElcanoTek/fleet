package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
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

// A leftover ORCHESTRATOR_SERVER_TOKEN (the retired Next tier's env; an old
// bootstrap even wrote ADMIN_API_KEY there) must not override the shared
// secret the in-process orchestrator verifies, or every call would 403.
func TestWebServerUsesTheSharedTokenForBothBackends(t *testing.T) {
	const secret = "web-test-session-secret-0123456789"
	t.Setenv("APP_SESSION_SECRET", secret)
	t.Setenv("ORCHESTRATOR_SERVER_TOKEN", "stale-admin-key")
	t.Setenv("FLEET_WEB_ADDR", "")
	var gotOrch, gotChat string
	orch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrch = r.Header.Get("X-Orchestrator-Server-Token")
		_, _ = io.WriteString(w, `{}`)
	})
	chat := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotChat = r.Header.Get("X-Chat-Server-Token")
		_, _ = io.WriteString(w, `{"epoch":"e1"}`)
	})
	srv, err := buildWebServer(&config.Config{SharedToken: "the-shared-token"}, chat, orch)
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
	if gotOrch != "the-shared-token" || gotChat != "the-shared-token" {
		t.Fatalf("orchestrator token %q, chat token %q; want the shared token for both", gotOrch, gotChat)
	}
}
