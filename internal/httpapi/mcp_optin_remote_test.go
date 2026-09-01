package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/secretbox"
	"github.com/ElcanoTek/fleet/internal/store"
)

// catalogEngine is a fakeEngine whose Optional-server catalog is fixed, so a
// test can exercise the bundle+remote whitelist merge without a Manager.
type catalogEngine struct {
	fakeEngine
	catalog  []agent.OptionalServerInfo
	alwaysOn []agent.AlwaysOnServerInfo
}

func (c *catalogEngine) MCPServerCatalog() []agent.OptionalServerInfo { return c.catalog }
func (c *catalogEngine) AlwaysOnMCPServerCatalog() []agent.AlwaysOnServerInfo {
	return c.alwaysOn
}

// remoteMCPFixture wires serverFixture's Postgres-backed Server with a real
// remotemcp.Service over the SAME store (cipher installed so Enabled() is
// true) and one remote server row named "GitHub" (mixed case on purpose —
// AddServer only trims names) owned by user. Skips without a test DSN.
func remoteMCPFixture(t *testing.T) (*Server, *store.Store, *store.RemoteMCPServer, string) {
	t.Helper()
	const user = "u@x.com"
	srv := serverFixture(t)
	srv.agent = &catalogEngine{catalog: []agent.OptionalServerInfo{{Name: "gamma"}}}

	st, ok := srv.store.(*store.Store)
	if !ok {
		t.Fatalf("serverFixture store is %T, want *store.Store", srv.store)
	}
	key := make([]byte, secretbox.KeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("key: %v", err)
	}
	cipher, err := secretbox.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	st.SetTokenCipher(cipher)
	srv.remoteMCP = remotemcp.NewService(st, remotemcp.Config{PublicBaseURL: "https://fleet.example.com"})
	if !srv.remoteMCP.Enabled() {
		t.Fatal("remote MCP service should be enabled with cipher + base URL")
	}

	row, err := st.CreateRemoteMCPServer(context.Background(), store.RemoteMCPServerInput{
		UserEmail:             user,
		Name:                  "GitHub",
		URL:                   "https://mcp.github.example.com",
		Transport:             store.RemoteMCPTransportStreamableHTTP,
		Issuer:                "https://auth.github.example.com",
		AuthorizationEndpoint: "https://auth.github.example.com/authorize",
		TokenEndpoint:         "https://auth.github.example.com/token",
		ClientID:              "client-1",
		ClientSecret:          "shh-secret",
	})
	if err != nil {
		t.Fatalf("CreateRemoteMCPServer: %v", err)
	}
	return srv, st, row, user
}

// A user's remote (hosted) MCP server must be persistable into a
// conversation's opt-in list: before the whitelist merge, the intersection
// against the bundle-only catalog silently dropped remote names, so a
// connected server could never participate in a chat turn (#443/#449).
func TestConversationMCPServersPOSTAcceptsRemoteServer(t *testing.T) {
	srv, st, _, user := remoteMCPFixture(t)
	ctx := context.Background()

	conv, err := st.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	body := strings.NewReader(`{"enabled_optional":["GitHub","gamma","nope"]}`)
	req := httptest.NewRequest("POST", "/conversations/"+conv.ID+"/mcp-servers", body)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	w := httptest.NewRecorder()
	srv.conversationByID(w, req)
	if w.Code != 200 {
		t.Fatalf("POST mcp-servers: status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		EnabledOptional []string `json:"enabled_optional"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	// Canonical lowercase, sorted; the unknown name is dropped, the remote
	// and bundle names both survive.
	want := []string{"gamma", "github"}
	if len(resp.EnabledOptional) != 2 || resp.EnabledOptional[0] != want[0] || resp.EnabledOptional[1] != want[1] {
		t.Errorf("enabled_optional = %v, want %v", resp.EnabledOptional, want)
	}

	got, err := st.Get(ctx, user, conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.OptionalMCPServersEnabled) != 2 || got.OptionalMCPServersEnabled[1] != "github" {
		t.Errorf("persisted opt-in list = %v, want [gamma github]", got.OptionalMCPServersEnabled)
	}
}

// Another user's remote server name must NOT become valid for the caller
// unless it was shared with them (#443 follow-up): the whitelist is scoped to
// the requesting user's own rows plus rows shared with them.
func TestConversationMCPServersPOSTScopedToCaller(t *testing.T) {
	srv, st, row, owner := remoteMCPFixture(t)
	ctx := context.Background()
	const other = "other@x.com"

	conv, err := st.CreateConversation(ctx, other, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	postGitHub := func() []string {
		t.Helper()
		body := strings.NewReader(`{"enabled_optional":["GitHub"]}`)
		req := httptest.NewRequest("POST", "/conversations/"+conv.ID+"/mcp-servers", body)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, other))
		w := httptest.NewRecorder()
		srv.conversationByID(w, req)
		if w.Code != 200 {
			t.Fatalf("POST mcp-servers: status %d body %s", w.Code, w.Body.String())
		}
		var resp struct {
			EnabledOptional []string `json:"enabled_optional"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("response: %v", err)
		}
		return resp.EnabledOptional
	}

	if got := postGitHub(); len(got) != 0 {
		t.Errorf("enabled_optional = %v, want [] — GitHub belongs to a different user and is not shared", got)
	}

	// Once the owner shares the server with the caller, its name becomes
	// valid — a shared connection is usable in the caller's runs, so the
	// opt-in whitelist must accept it too.
	if err := st.ShareRemoteMCPServer(ctx, owner, row.ID, other); err != nil {
		t.Fatalf("ShareRemoteMCPServer: %v", err)
	}
	if got := postGitHub(); len(got) != 1 || got[0] != "github" {
		t.Errorf("enabled_optional = %v, want [github] — GitHub is shared with the caller", got)
	}
}

// The per-conversation Tools-picker catalog must list the caller's CONNECTED
// remote servers as toggleable entries (mirroring the startup catalog merge),
// with the enabled flag reflecting the canonically-lowercased opt-in list.
func TestConversationMCPServersGETListsRemoteServer(t *testing.T) {
	srv, st, row, user := remoteMCPFixture(t)
	ctx := context.Background()

	if err := st.SetRemoteMCPStatus(ctx, user, row.ID, store.RemoteMCPStatusConnected, ""); err != nil {
		t.Fatalf("SetRemoteMCPStatus: %v", err)
	}
	conv, err := st.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := st.SetOptionalMCPServers(ctx, user, conv.ID, []string{"github"}); err != nil {
		t.Fatalf("SetOptionalMCPServers: %v", err)
	}

	req := httptest.NewRequest("GET", "/conversations/"+conv.ID+"/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	w := httptest.NewRecorder()
	srv.conversationByID(w, req)
	if w.Code != 200 {
		t.Fatalf("GET mcp-servers: status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	var remote map[string]any
	for _, entry := range resp.Servers {
		if entry["name"] == "GitHub" {
			remote = entry
			break
		}
	}
	if remote == nil {
		t.Fatalf("remote server missing from per-conversation catalog: %v", resp.Servers)
	}
	if remote["remote"] != true {
		t.Errorf("remote flag = %v, want true", remote["remote"])
	}
	if remote["enabled"] != true {
		t.Errorf("enabled = %v, want true (opt-in list holds the lowercased name)", remote["enabled"])
	}
}
