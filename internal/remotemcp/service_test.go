package remotemcp

import (
	"context"
	"encoding/json"
	"errors"
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
	// apiKeys: server id -> plaintext key (the real store seals it).
	apiKeys map[string]string
	nextID  int
	// shares: server id -> grantees ('*' or emails), mirroring the real table.
	shares map[string][]string
	// prefs: email -> connector-pref map, mirroring user_connector_prefs.
	prefs map[string]map[string]store.ConnectorPref
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		servers: map[string]*store.RemoteMCPServer{},
		tokens:  map[string]store.RemoteMCPTokens{},
		flows:   map[string]*store.OAuthFlowState{},
		apiKeys: map[string]string{},
	}
}

func (f *fakeStore) RemoteMCPEncryptionEnabled() bool { return true }

func (f *fakeStore) CreateRemoteMCPServer(_ context.Context, in store.RemoteMCPServerInput) (*store.RemoteMCPServer, error) {
	f.nextID++
	id := "srv-" + string(rune('a'+f.nextID))
	srv := &store.RemoteMCPServer{
		ID: id, UserEmail: strings.ToLower(in.UserEmail), Name: in.Name, URL: in.URL,
		Transport: in.Transport, Status: in.Status,
		Issuer: in.Issuer, AuthorizationEndpoint: in.AuthorizationEndpoint, TokenEndpoint: in.TokenEndpoint,
		RegistrationEndpoint: in.RegistrationEndpoint, RevocationEndpoint: in.RevocationEndpoint,
		Scopes: in.Scopes, AuthMethods: in.AuthMethods, ClientID: in.ClientID,
		AuthKind: in.AuthKind, APIKeyHeader: in.APIKeyHeader,
	}
	if srv.Status == "" {
		srv.Status = store.RemoteMCPStatusLoginRequired
	}
	if in.APIKey != "" {
		f.apiKeys[id] = in.APIKey
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

func (f *fakeStore) GetRemoteMCPAPIKey(_ context.Context, srv *store.RemoteMCPServer) (string, error) {
	if _, ok := f.servers[srv.ID]; !ok {
		return "", store.ErrRemoteMCPNotFound
	}
	return f.apiKeys[srv.ID], nil
}

func (f *fakeStore) SetRemoteMCPAPIKey(_ context.Context, email, id, apiKey string) error {
	s, ok := f.servers[id]
	if !ok || !strings.EqualFold(s.UserEmail, email) {
		return store.ErrRemoteMCPNotFound
	}
	if s.AuthKind != store.RemoteMCPAuthAPIKey {
		return errors.New("this connection does not use an API key")
	}
	f.apiKeys[id] = apiKey
	s.Status = store.RemoteMCPStatusConnected
	return nil
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

func (f *fakeStore) ShareRemoteMCPServer(_ context.Context, owner, id, grantee string) error {
	srv, ok := f.servers[id]
	if !ok || srv.UserEmail != owner {
		return store.ErrRemoteMCPNotFound
	}
	if f.shares == nil {
		f.shares = map[string][]string{}
	}
	f.shares[id] = append(f.shares[id], grantee)
	return nil
}

func (f *fakeStore) UnshareRemoteMCPServer(_ context.Context, owner, id, grantee string) error {
	srv, ok := f.servers[id]
	if !ok || srv.UserEmail != owner {
		return store.ErrRemoteMCPNotFound
	}
	out := f.shares[id][:0]
	found := false
	for _, g := range f.shares[id] {
		if g == grantee {
			found = true
			continue
		}
		out = append(out, g)
	}
	if !found {
		return store.ErrRemoteMCPNotFound
	}
	f.shares[id] = out
	return nil
}

func (f *fakeStore) ListRemoteMCPSharesByOwner(_ context.Context, owner string) (map[string][]string, error) {
	out := map[string][]string{}
	for id, gs := range f.shares {
		if srv, ok := f.servers[id]; ok && srv.UserEmail == owner {
			out[id] = append([]string{}, gs...)
		}
	}
	return out, nil
}

func (f *fakeStore) sharedWith(email string) []*store.RemoteMCPServer {
	var out []*store.RemoteMCPServer
	for id, gs := range f.shares {
		srv, ok := f.servers[id]
		if !ok || srv.UserEmail == email {
			continue
		}
		for _, g := range gs {
			if g == email || g == store.GranteeEveryone {
				out = append(out, srv)
				break
			}
		}
	}
	return out
}

func (f *fakeStore) ListRemoteMCPServersSharedWith(_ context.Context, email string) ([]store.RemoteMCPServer, error) {
	shared := f.sharedWith(email)
	out := make([]store.RemoteMCPServer, 0, len(shared))
	for _, srv := range shared {
		out = append(out, *srv)
	}
	return out, nil
}

func (f *fakeStore) GetRemoteMCPServerForUse(_ context.Context, email, id string) (*store.RemoteMCPServer, error) {
	srv, ok := f.servers[id]
	if !ok {
		return nil, store.ErrRemoteMCPNotFound
	}
	if srv.UserEmail == email {
		cp := *srv
		return &cp, nil
	}
	for _, s2 := range f.sharedWith(email) {
		if s2.ID == id {
			cp := *s2
			return &cp, nil
		}
	}
	return nil, store.ErrRemoteMCPNotFound
}

func (f *fakeStore) ListConnectorPrefs(_ context.Context, email string) (map[string]store.ConnectorPref, error) {
	return f.prefs[strings.ToLower(email)], nil
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

func TestServiceAddOpenServerSkipsOAuthDiscovery(t *testing.T) {
	fs := newFakeStore()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Location", "https://example.com/docs")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	svc := newTestService(t, fs, srv)

	server, err := svc.AddServer(context.Background(), AddServerInput{
		Email: "u@x.com", Name: "aws-knowledge", URL: srv.URL, AuthMode: "open",
	})
	if err != nil {
		t.Fatalf("AddServer(open): %v", err)
	}
	if requests != 0 {
		t.Fatalf("open server add made %d discovery requests, want none", requests)
	}
	if server.Status != store.RemoteMCPStatusConnected || server.Issuer != "" {
		t.Errorf("open server = %+v, want connected without an OAuth issuer", server)
	}
	bearer, err := svc.AcquireTokenByID(context.Background(), "u@x.com", server.ID)
	if err != nil || bearer != "" {
		t.Errorf("AcquireTokenByID(open) = %q, %v; want empty bearer, nil", bearer, err)
	}
}

func TestServiceAddAPIKeyServerSkipsOAuthDiscovery(t *testing.T) {
	fs := newFakeStore()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	svc := newTestService(t, fs, srv)
	ctx := context.Background()

	// Missing key is rejected before anything is stored.
	if _, err := svc.AddServer(ctx, AddServerInput{
		Email: "u@x.com", Name: "zapier", URL: srv.URL, AuthMode: "api_key",
	}); err == nil {
		t.Fatal("AddServer(api_key) accepted an empty key")
	}
	// A reserved / malformed header name is rejected.
	for _, h := range []string{"Host", "Content-Length", "X Bad Header", "X-Key\r\nEvil: 1"} {
		if _, err := svc.AddServer(ctx, AddServerInput{
			Email: "u@x.com", Name: "zapier", URL: srv.URL, AuthMode: "api_key", APIKey: "sk-1", APIKeyHeader: h,
		}); err == nil {
			t.Errorf("AddServer(api_key) accepted header %q", h)
		}
	}
	// A key with control characters can never be sent as a header value.
	if _, err := svc.AddServer(ctx, AddServerInput{
		Email: "u@x.com", Name: "zapier", URL: srv.URL, AuthMode: "api_key", APIKey: "sk\r\nEvil: 1",
	}); err == nil {
		t.Error("AddServer(api_key) accepted a key with CRLF")
	}

	server, err := svc.AddServer(ctx, AddServerInput{
		Email: "u@x.com", Name: "zapier", URL: srv.URL, AuthMode: "api_key",
		APIKey: "sk-live-123", APIKeyHeader: "X-API-Key",
	})
	if err != nil {
		t.Fatalf("AddServer(api_key): %v", err)
	}
	if requests != 0 {
		t.Fatalf("api_key add made %d discovery requests, want none", requests)
	}
	if server.Status != store.RemoteMCPStatusConnected || server.AuthKind != store.RemoteMCPAuthAPIKey {
		t.Errorf("api_key server = %+v, want connected with auth_kind api_key", server)
	}
	if server.APIKeyHeader != "X-API-Key" {
		t.Errorf("api_key header = %q", server.APIKeyHeader)
	}

	// The run loop's credential path returns the key itself.
	cred, err := svc.AcquireTokenByID(ctx, "u@x.com", server.ID)
	if err != nil || cred != "sk-live-123" {
		t.Errorf("AcquireTokenByID(api_key) = %q, %v; want the key, nil", cred, err)
	}

	// Rotation replaces the key and reconnects; only the owner's api_key rows.
	if err := svc.SetAPIKey(ctx, "u@x.com", server.ID, "sk-live-456"); err != nil {
		t.Fatalf("SetAPIKey: %v", err)
	}
	if cred, _ := svc.AcquireTokenByID(ctx, "u@x.com", server.ID); cred != "sk-live-456" {
		t.Errorf("rotated key = %q", cred)
	}
	if err := svc.SetAPIKey(ctx, "other@x.com", server.ID, "steal"); !errors.Is(err, store.ErrRemoteMCPNotFound) {
		t.Errorf("SetAPIKey by non-owner = %v, want not-found", err)
	}
	if err := svc.SetAPIKey(ctx, "u@x.com", server.ID, ""); err == nil {
		t.Error("SetAPIKey accepted an empty key")
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

// Sharing (#443 follow-up): shared servers resolve for the grantee's runs with
// owner attribution, an own-name collision wins over a shared server, and the
// token minted for a shared server is the OWNER's.
func TestResolverIncludesSharedServers(t *testing.T) {
	fs := newFakeStore()
	srv := oauthTestServer(t, "rotate")
	svc := newTestService(t, fs, srv)
	ctx := context.Background()

	// Owner connects a server, then shares it with mate.
	owned, _ := svc.AddServer(ctx, AddServerInput{Email: "owner@x.com", Name: "acme", URL: srv.URL + "/mcp"})
	authURL, _ := svc.Authorize(ctx, "owner@x.com", owned.ID)
	_ = authURL
	var state string
	for k := range fs.flows {
		state = k
	}
	if _, err := svc.Complete(ctx, "owner@x.com", state, "c"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := svc.ShareServer(ctx, "owner@x.com", owned.ID, "mate@x.com"); err != nil {
		t.Fatalf("share: %v", err)
	}

	conns, err := svc.ConnectedServersForUser(ctx, "mate@x.com")
	if err != nil {
		t.Fatalf("connected for mate: %v", err)
	}
	if len(conns) != 1 || conns[0].Name != "acme" || conns[0].Owner != "owner@x.com" {
		t.Fatalf("mate should see the shared server with owner attribution, got %+v", conns)
	}

	// The grantee can mint a token — it is the owner's token, brokered host-side.
	bearer, err := svc.AcquireTokenByID(ctx, "mate@x.com", owned.ID)
	if err != nil {
		t.Fatalf("acquire via share: %v", err)
	}
	if bearer == "" {
		t.Fatal("empty bearer")
	}
	// An ungranted user cannot.
	if _, err := svc.AcquireTokenByID(ctx, "other@x.com", owned.ID); err == nil {
		t.Error("ungranted user acquired a token")
	}

	// Name collision: mate's OWN server named acme wins; the shared one is skipped.
	mates, _ := svc.AddServer(ctx, AddServerInput{Email: "mate@x.com", Name: "acme", URL: srv.URL + "/mcp"})
	authURL, _ = svc.Authorize(ctx, "mate@x.com", mates.ID)
	_ = authURL
	for k := range fs.flows {
		state = k
	}
	if _, err := svc.Complete(ctx, "mate@x.com", state, "c"); err != nil {
		t.Fatalf("Complete mate: %v", err)
	}
	conns, _ = svc.ConnectedServersForUser(ctx, "mate@x.com")
	if len(conns) != 1 || conns[0].ID != mates.ID || conns[0].Owner != "" {
		t.Fatalf("own server must win the name collision, got %+v", conns)
	}
}
