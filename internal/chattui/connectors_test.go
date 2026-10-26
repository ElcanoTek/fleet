package chattui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// connectorServer answers GET /mcp-servers with catalog (or catalogStatus)
// and POST /chat with a minimal stream, recording each /chat body.
type connectorServer struct {
	mu            sync.Mutex
	catalogHits   int
	chatBodies    []map[string]any
	catalogStatus int
}

func (s *connectorServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	catalog := map[string]any{"servers": []map[string]any{
		{"name": "email", "enabled": true, "status": "available"},
		{"name": "lens", "enabled": false, "status": "available"},
		{"name": "pages", "enabled": true, "status": "available"},
		{"name": "plugin_notes", "enabled": true, "always_on": true},
		{"name": "fastio", "enabled": false, "status": "needs_setup"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/mcp-servers":
			s.catalogHits++
			if s.catalogStatus != 0 {
				w.WriteHeader(s.catalogStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(catalog)
		case "/chat":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			s.chatBodies = append(s.chatBodies, body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: conversation\ndata: {\"id\":\"conv-1\"}\n\nevent: turn.completed\ndata: {}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *connectorServer) lastEnabled(t *testing.T) ([]string, bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chatBodies) == 0 {
		t.Fatal("no /chat request was sent")
	}
	raw, ok := s.chatBodies[len(s.chatBodies)-1]["enabled_optional"]
	if !ok {
		return nil, false
	}
	var names []string
	for _, n := range raw.([]any) {
		names = append(names, n.(string))
	}
	return names, true
}

func TestNewConversationOpensWithTheWebChatsDefaultConnectors(t *testing.T) {
	s := &connectorServer{}
	srv := s.start(t)
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "tok", DefaultConnectors: true})
	if _, err := c.Stream(context.Background(), "hi", "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.lastEnabled(t)
	// enabled and not always-on, exactly as the web's enabledOptionalMcpServerNames.
	if !ok || !slices.Equal(got, []string{"email", "pages"}) {
		t.Fatalf("enabled_optional = %v (sent %v), want [email pages]", got, ok)
	}
}

func TestExplicitConnectorsReplaceTheDefaults(t *testing.T) {
	s := &connectorServer{}
	srv := s.start(t)
	cfg, err := Resolve(Flags{Server: srv.URL, Email: "u@x.co", Connectors: []string{" Lens ", "lens", "EMAIL"}},
		func(k string) string {
			if k == "FLEET_SERVER_TOKEN" {
				return "tok"
			}
			return ""
		}, func(string) ([]byte, error) { return nil, io.EOF }, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultConnectors = true
	if _, err := NewClient(cfg).Stream(context.Background(), "hi", "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.lastEnabled(t); !slices.Equal(got, []string{"lens", "email"}) {
		t.Fatalf("enabled_optional = %v, want [lens email] (normalized, de-duplicated, replacing defaults)", got)
	}
}

func TestAnUnknownOrUnusableConnectorIsRefusedBeforeAnyTurn(t *testing.T) {
	for _, name := range []string{"nope", "plugin_notes", "fastio"} {
		s := &connectorServer{}
		srv := s.start(t)
		c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "tok", DefaultConnectors: true,
			Connectors: []string{name}})
		_, err := c.Stream(context.Background(), "hi", "", func(Event) {})
		if err == nil || !strings.Contains(err.Error(), "not an optional connector you can use here") {
			t.Fatalf("%s: err = %v", name, err)
		}
		if len(s.chatBodies) != 0 {
			t.Fatalf("%s: a turn was sent anyway", name)
		}
	}
}

func TestAResumedConversationKeepsItsOwnConnectors(t *testing.T) {
	s := &connectorServer{}
	srv := s.start(t)
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "tok", DefaultConnectors: true})
	if _, err := c.Stream(context.Background(), "hi", "conv-existing", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if _, sent := s.lastEnabled(t); sent || s.catalogHits != 0 {
		t.Fatalf("resumed turn sent enabled_optional=%v, catalog hits=%d; want neither", sent, s.catalogHits)
	}
}

func TestClientsThatDoNotAskForDefaultsAreUnchanged(t *testing.T) {
	// fleet acp builds its Config without DefaultConnectors.
	s := &connectorServer{}
	srv := s.start(t)
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "tok"})
	if _, err := c.Stream(context.Background(), "hi", "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if _, sent := s.lastEnabled(t); sent || s.catalogHits != 0 {
		t.Fatalf("enabled_optional sent=%v, catalog hits=%d; want neither", sent, s.catalogHits)
	}
}

func TestAnUnreadableCatalogStopsTheTurnInsteadOfStartingWithoutConnectors(t *testing.T) {
	s := &connectorServer{catalogStatus: http.StatusInternalServerError}
	srv := s.start(t)
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "tok", DefaultConnectors: true})
	_, err := c.Stream(context.Background(), "hi", "", func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "connector catalog") {
		t.Fatalf("err = %v, want a connector catalog error", err)
	}
	if len(s.chatBodies) != 0 {
		t.Fatal("a turn was sent without its connectors")
	}
}

func TestABlankConnectorIsAUsageErrorNotTheDefaults(t *testing.T) {
	for _, v := range []string{"", "   "} {
		if code := Run([]string{"--connector", v, "--message", "hi"}); code != 2 {
			t.Fatalf("--connector %q: exit code = %d, want 2", v, code)
		}
	}
}

func TestConnectorFlagWithAnExistingConversationIsAUsageError(t *testing.T) {
	if code := Run([]string{"--connector", "lens", "--conversation", "conv-1", "--message", "hi"}); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
