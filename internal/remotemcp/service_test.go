package remotemcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// fakeStore is an in-memory tokenStore for exercising service orchestration
// without a database. It does not encrypt (the real encryption is covered by
// the store package tests); it focuses on the service's flow logic.
type fakeStore struct {
	servers map[string]*store.RemoteMCPServer
	tokens  map[string]store.RemoteMCPTokens
	flows   map[string]*store.OAuthFlowState
	nextID  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		servers: map[string]*store.RemoteMCPServer{},
		tokens:  map[string]store.RemoteMCPTokens{},
		flows:   map[string]*store.OAuthFlowState{},
	}
}

func (f *fakeStore) RemoteMCPEncryptionEnabled() bool { return true }

func (f *fakeStore) CreateRemoteMCPServer(_ context.Context, in store.RemoteMCPServerInput) (*store.RemoteMCPServer, error) {
	f.nextID++
	id := "srv-" + string(rune('a'+f.nextID))
	srv := &store.RemoteMCPServer{
		ID: id, UserEmail: strings.ToLower(in.UserEmail), Name: in.Name, URL: in.URL,
		Transport: in.Transport, Status: store.RemoteMCPStatusLoginRequired,
		Issuer: in.Issuer, AuthorizationEndpoint: in.AuthorizationEndpoint, TokenEndpoint: in.TokenEndpoint,
		RegistrationEndpoint: in.RegistrationEndpoint, RevocationEndpoint: in.RevocationEndpoint,
		Scopes: in.Scopes, AuthMethods: in.AuthMethods, ClientID: in.ClientID,
	}
	f.servers[id] = srv
	cp := *srv
	return &cp, nil
}

func (f *fakeStore) GetRemoteMCPServer(_ context.Context, email, id string) (*store.RemoteMCPServer, error) {
	s, ok := f.servers[id]
	if !ok || !strings.EqualFold(s.UserEmail, email) {
		return nil, store.ErrRemoteMCPNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeStore) ListRemoteMCPServers(_ context.Context, email string) ([]store.RemoteMCPServer, error) {
	var out []store.RemoteMCPServer
	for _, s := range f.servers {
		if strings.EqualFold(s.UserEmail, email) {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteRemoteMCPServer(_ context.Context, email, id string) error {
	s, ok := f.servers[id]
	if !ok || !strings.EqualFold(s.UserEmail, email) {
		return store.ErrRemoteMCPNotFound
	}
	delete(f.servers, id)
	delete(f.tokens, id)
	return nil
}

func (f *fakeStore) LoadServerSecrets(_ context.Context, _ *store.RemoteMCPServer) (string, string, error) {
	return "", "", nil
}

func (f *fakeStore) GetOAuthTokens(_ context.Context, srv *store.RemoteMCPServer) (*store.RemoteMCPTokens, error) {
	t, ok := f.tokens[srv.ID]
	if !ok {
		return nil, store.ErrRemoteMCPNeedsReauth
	}
	cp := t
	return &cp, nil
}

func (f *fakeStore) StoreOAuthTokens(_ context.Context, srv *store.RemoteMCPServer, t store.RemoteMCPTokens) error {
	f.tokens[srv.ID] = t
	if s := f.servers[srv.ID]; s != nil {
		s.Status = store.RemoteMCPStatusConnected
	}
	return nil
}

func (f *fakeStore) EnsureFreshToken(ctx context.Context, srv *store.RemoteMCPServer, margin int64, fn store.RefreshFunc) (string, error) {
	t, ok := f.tokens[srv.ID]
	if !ok || t.RefreshToken == "" {
		return "", store.ErrRemoteMCPNeedsReauth
	}
	if t.AccessToken != "" && t.ExpiresAt != 0 && t.ExpiresAt-time.Now().Unix() > margin {
		return t.AccessToken, nil
	}
	res, err := fn(ctx, t)
	if err != nil {
		return "", err
	}
	if res.NeedReauth {
		if s := f.servers[srv.ID]; s != nil {
			s.Status = store.RemoteMCPStatusNeedsReauth
		}
		return "", store.ErrRemoteMCPNeedsReauth
	}
	f.tokens[srv.ID] = res.Tokens
	return res.Tokens.AccessToken, nil
}

func (f *fakeStore) BeginOAuthFlow(_ context.Context, state, serverID, email, verifier string, _ time.Duration) error {
	f.flows[state] = &store.OAuthFlowState{ServerID: serverID, UserEmail: strings.ToLower(email), CodeVerifier: verifier}
	return nil
}

func (f *fakeStore) ConsumeOAuthFlow(_ context.Context, state string) (*store.OAuthFlowState, error) {
	fl, ok := f.flows[state]
	if !ok {
		return nil, store.ErrOAuthFlowNotFound
	}
	delete(f.flows, state) // single-use
	return fl, nil
}

// oauthTestServer is an MCP server + authorization server in one httptest server.
func oauthTestServer(t *testing.T, refreshBehavior string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              base + "/mcp",
			"authorization_servers": []string{base},
			"scopes_supported":      []string{"mcp:read"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           base,
			"authorization_endpoint":           base + "/authorize",
			"token_endpoint":                   base + "/token",
			"registration_endpoint":            base + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "dyn-client"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-init", "refresh_token": "rt-init", "token_type": "Bearer", "expires_in": 1})
		case "refresh_token":
			if refreshBehavior == "invalid_grant" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-refreshed", "refresh_token": "rt-rotated", "token_type": "Bearer", "expires_in": 3600})
		}
	})
	return srv
}

func newTestService(t *testing.T, fs *fakeStore, srv *httptest.Server) *Service {
	t.Helper()
	svc := &Service{
		store:      fs,
		cfg:        Config{PublicBaseURL: "https://fleet.example.com", AllowInsecureHTTP: true},
		httpClient: srv.Client(), // bypass SSRF guard for loopback httptest
	}
	svc.cfg.withDefaults()
	return svc
}

func TestServiceAddAuthorizeComplete(t *testing.T) {
	fs := newFakeStore()
	srv := oauthTestServer(t, "rotate")
	svc := newTestService(t, fs, srv)
	ctx := context.Background()

	server, err := svc.AddServer(ctx, AddServerInput{Email: "u@x.com", Name: "acme", URL: srv.URL + "/mcp"})
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if server.ClientID != "dyn-client" {
		t.Errorf("client_id = %q (expected DCR result)", server.ClientID)
	}
	if server.URL != srv.URL+"/mcp" {
		t.Errorf("server URL = %q", server.URL)
	}

	authURL, err := svc.Authorize(ctx, "u@x.com", server.ID)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !strings.Contains(authURL, "code_challenge_method=S256") || !strings.Contains(authURL, "resource=") {
		t.Errorf("auth URL missing PKCE/resource: %s", authURL)
	}
	// Exactly one flow stored.
	if len(fs.flows) != 1 {
		t.Fatalf("expected 1 flow, got %d", len(fs.flows))
	}
	var state string
	for k := range fs.flows {
		state = k
	}

	// Wrong user must be rejected.
	if _, err := svc.Complete(ctx, "attacker@x.com", state, "code"); err == nil {
		t.Error("Complete accepted a mismatched user")
	}

	// Re-create the flow (the rejected attempt consumed it).
	authURL, _ = svc.Authorize(ctx, "u@x.com", server.ID)
	_ = authURL
	for k := range fs.flows {
		state = k
	}
	done, err := svc.Complete(ctx, "u@x.com", state, "code-123")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.Status != store.RemoteMCPStatusConnected {
		t.Errorf("status = %q", done.Status)
	}
	if fs.tokens[server.ID].AccessToken != "at-init" {
		t.Errorf("stored access token = %q", fs.tokens[server.ID].AccessToken)
	}
}

func TestServiceAcquireTokenRefreshes(t *testing.T) {
	fs := newFakeStore()
	srv := oauthTestServer(t, "rotate")
	svc := newTestService(t, fs, srv)
	ctx := context.Background()

	server, _ := svc.AddServer(ctx, AddServerInput{Email: "u@x.com", Name: "acme", URL: srv.URL + "/mcp"})
	// Store a near-expiry token (expires_in:1 from the exchange).
	authURL, _ := svc.Authorize(ctx, "u@x.com", server.ID)
	_ = authURL
	var state string
	for k := range fs.flows {
		state = k
	}
	if _, err := svc.Complete(ctx, "u@x.com", state, "c"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // let the 1s access token go stale

	server, _ = svc.store.GetRemoteMCPServer(ctx, "u@x.com", server.ID)
	bearer, err := svc.AcquireToken(ctx, server)
	if err != nil {
		t.Fatalf("AcquireToken: %v", err)
	}
	if bearer != "at-refreshed" {
		t.Errorf("bearer = %q, want refreshed token", bearer)
	}
}

func TestServiceAcquireTokenNeedsReauth(t *testing.T) {
	fs := newFakeStore()
	srv := oauthTestServer(t, "invalid_grant")
	svc := newTestService(t, fs, srv)
	ctx := context.Background()

	server, _ := svc.AddServer(ctx, AddServerInput{Email: "u@x.com", Name: "acme", URL: srv.URL + "/mcp"})
	authURL, _ := svc.Authorize(ctx, "u@x.com", server.ID)
	_ = authURL
	var state string
	for k := range fs.flows {
		state = k
	}
	_, _ = svc.Complete(ctx, "u@x.com", state, "c")
	time.Sleep(1100 * time.Millisecond)

	server, _ = svc.store.GetRemoteMCPServer(ctx, "u@x.com", server.ID)
	_, err := svc.AcquireToken(ctx, server)
	if err == nil {
		t.Fatal("expected needs-reauth error")
	}
	server, _ = svc.store.GetRemoteMCPServer(ctx, "u@x.com", server.ID)
	if server.Status != store.RemoteMCPStatusNeedsReauth {
		t.Errorf("status = %q, want needs_reauth", server.Status)
	}
}
