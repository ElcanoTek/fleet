// Package remotemcp orchestrates the per-user remote (hosted) MCP + OAuth
// feature (#443): adding a server from the GUI, running the OAuth 2.1 + PKCE
// login flow, and handing freshly-refreshed bearer tokens to the agent run
// loop. It ties together internal/store (encrypted persistence + the refresh
// row-lock), internal/mcpoauth (discovery, DCR, token flow, SSRF-safe HTTP),
// and the deployment config (the stable redirect URI).
//
// SECURITY INVARIANTS this package upholds:
//   - Credentials stay host-side. Bearer/refresh tokens live only in this
//     process and the Postgres token store (encrypted). They are returned to
//     the run loop solely as the Authorization header on the host-side MCP HTTP
//     request — never into the sandbox, model context, or logs.
//   - The canonical server URI (mcpoauth.CanonicalResourceURI) is the single
//     identity used for the DB key, the encryption AAD, the OAuth state, the
//     RFC 8707 resource indicator, and broker routing.
//   - Every operation is scoped to the caller's email; one user can never act on
//     another's connection. The OAuth callback additionally requires the
//     completing user to equal the user who initiated the flow.
package remotemcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
	"github.com/ElcanoTek/fleet/internal/store"
)

// ErrDisabled is returned when the feature can't operate: no encryption key, or
// no public base URL to build the OAuth redirect URI from.
var ErrDisabled = errors.New("remote MCP OAuth is not configured")

// ErrManualClientRequired is returned when an authorization server offers no
// dynamic client registration and the user supplied no manual client_id.
var ErrManualClientRequired = errors.New("authorization server does not support dynamic client registration; provide a client_id (and secret) manually")

// tokenStore is the slice of *store.Store this package needs (kept narrow so it
// is easy to fake in tests).
type tokenStore interface {
	RemoteMCPEncryptionEnabled() bool
	CreateRemoteMCPServer(ctx context.Context, in store.RemoteMCPServerInput) (*store.RemoteMCPServer, error)
	GetRemoteMCPServer(ctx context.Context, userEmail, id string) (*store.RemoteMCPServer, error)
	ListRemoteMCPServers(ctx context.Context, userEmail string) ([]store.RemoteMCPServer, error)
	DeleteRemoteMCPServer(ctx context.Context, userEmail, id string) error
	LoadServerSecrets(ctx context.Context, server *store.RemoteMCPServer) (clientSecret, registrationToken string, err error)
	GetRemoteMCPAPIKey(ctx context.Context, server *store.RemoteMCPServer) (string, error)
	SetRemoteMCPAPIKey(ctx context.Context, userEmail, id, apiKey string) error
	// Seats (#988).
	SetRemoteMCPDefaultSeat(ctx context.Context, userEmail, id string) error
	RenameRemoteMCPAccount(ctx context.Context, userEmail, id, label string) error
	GetOAuthTokens(ctx context.Context, server *store.RemoteMCPServer) (*store.RemoteMCPTokens, error)
	ClearOAuthTokens(ctx context.Context, userEmail, id string) error
	StoreOAuthTokens(ctx context.Context, server *store.RemoteMCPServer, tokens store.RemoteMCPTokens) error
	EnsureFreshToken(ctx context.Context, server *store.RemoteMCPServer, marginSeconds int64, refreshFn store.RefreshFunc) (string, error)
	BeginOAuthFlow(ctx context.Context, state, serverID, userEmail, codeVerifier string, ttl time.Duration) error
	ConsumeOAuthFlow(ctx context.Context, state string) (*store.OAuthFlowState, error)
	// Sharing (#443 follow-up).
	ShareRemoteMCPServer(ctx context.Context, ownerEmail, serverID, grantee string) error
	UnshareRemoteMCPServer(ctx context.Context, ownerEmail, serverID, grantee string) error
	ListRemoteMCPSharesByOwner(ctx context.Context, ownerEmail string) (map[string][]string, error)
	ListRemoteMCPServersSharedWith(ctx context.Context, email string) ([]store.RemoteMCPServer, error)
	GetRemoteMCPServerForUse(ctx context.Context, email, id string) (*store.RemoteMCPServer, error)
	// Per-user connector availability prefs (unified connector UX).
	ListConnectorPrefs(ctx context.Context, userEmail string) (map[string]store.ConnectorPref, error)
}

// Config is the deployment configuration for the feature.
type Config struct {
	// PublicBaseURL is the externally-reachable origin of the web app, e.g.
	// "https://fleet.example.com". The OAuth redirect URI is derived from it and
	// must be byte-stable across DCR, authorization, and token requests, so it is
	// taken from config and NEVER reconstructed from request headers.
	PublicBaseURL string
	// CallbackPath is the browser-facing OAuth callback route (on the Next.js
	// app), e.g. "/api/oauth/mcp/callback".
	CallbackPath string
	// ClientName is the human label registered via DCR / shown on consent.
	ClientName string
	// AllowInsecureHTTP permits http:// servers (dev/test only).
	AllowInsecureHTTP bool
	// RefreshMargin refreshes a token expiring within this window. Default 5m.
	RefreshMargin time.Duration
	// FlowTTL bounds how long an in-flight authorization may take. Default 10m.
	FlowTTL time.Duration
	// HTTPTimeout bounds each outbound control-plane request. Default 30s.
	HTTPTimeout time.Duration
	// RefreshTimeout bounds the token refresh held under the DB row lock. Default 10s.
	RefreshTimeout time.Duration
}

func (c *Config) withDefaults() {
	if c.CallbackPath == "" {
		c.CallbackPath = "/api/oauth/mcp/callback"
	}
	if c.ClientName == "" {
		c.ClientName = "fleet"
	}
	if c.RefreshMargin <= 0 {
		c.RefreshMargin = 5 * time.Minute
	}
	if c.FlowTTL <= 0 {
		c.FlowTTL = 10 * time.Minute
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 30 * time.Second
	}
	if c.RefreshTimeout <= 0 {
		c.RefreshTimeout = 10 * time.Second
	}
}

// Service is the per-user remote-MCP orchestrator.
type Service struct {
	store      tokenStore
	cfg        Config
	httpClient *http.Client // SSRF-safe; used for all control-plane OAuth requests

	// observeSecret, when set (SetSecretObserver), receives every
	// runtime-acquired credential so the host process can register it for
	// literal redaction (#1124). Never called with an empty value; never
	// logged here.
	observeSecret SecretObserver
}

// SecretObserver receives credentials the Service acquires at RUNTIME.
//
// scope, when non-empty, names the ROTATING credential set the values belong to
// — one server row, whose OAuth access+refresh pair is replaced wholesale by
// each refresh. rotated says the values REPLACE that scope's previous
// generation, which is what lets the host redactor retire the rotated-out
// secrets after a grace window instead of scanning for every token the process
// has ever held (#1274): without it a broker on hourly-expiry tokens grew its
// literal set by ~2-3 entries per refresh per server, forever. rotated=false
// adds to the scope's CURRENT generation (a secret about to ride a request, a
// bearer returned unchanged) and retires nothing. An empty scope means the
// credential does not rotate on a clock (a static api_key, a client secret
// acquired before its row exists) and is registered for the process lifetime.
//
// fn must be safe for concurrent use and must ONLY register the values for
// redaction — never log, persist or forward them.
type SecretObserver func(scope string, rotated bool, secrets ...string)

// secretScope names the rotating credential set for one server row. The row is
// the right generation boundary: its tokens rotate together, and a rotation for
// one row must never retire another row's (or another user's) literals.
func secretScope(serverID string) string {
	if strings.TrimSpace(serverID) == "" {
		return ""
	}
	return "remotemcp:" + serverID
}

// NewService builds a Service. cfg is normalized; the SSRF-safe HTTP client is
// constructed once. The store must have a token cipher installed for the
// feature to be enabled.
func NewService(st *store.Store, cfg Config) *Service {
	cfg.withDefaults()
	return &Service{store: st, cfg: cfg, httpClient: mcpoauth.SafeHTTPClient(cfg.HTTPTimeout)}
}

// SetSecretObserver registers fn to receive every credential this service
// acquires at RUNTIME — minted/refreshed OAuth bearers, rotated refresh
// tokens, sealed api_key secrets — at the moment of acquisition, before the
// credential is used on any request. The credential-owning broker wires it to
// mcpbroker.RegisterSecretLiterals (the main process wires
// agentcore.RegisterSecretLiterals) so a connector echoing its own token into
// an error string is scrubbed by VALUE, like boot-time env secrets (#1124).
// See SecretObserver for the scope/rotated contract. Set during wiring, before
// the service serves traffic (the field itself is not synchronized).
func (s *Service) SetSecretObserver(fn SecretObserver) {
	s.observeSecret = fn
}

// noteSecrets registers non-rotating credentials for the process lifetime: a
// static api_key, a client secret or registration token acquired before its
// server row exists. No-op when no observer is registered.
func (s *Service) noteSecrets(values ...string) {
	s.observe("", false, values...)
}

// noteScopedSecrets registers credentials the server row is using RIGHT NOW,
// joining its current generation without superseding anything — so a secret
// still in play can never have its coverage shortened by this call.
func (s *Service) noteScopedSecrets(scope string, values ...string) {
	s.observe(scope, false, values...)
}

// noteRotatedSecrets records a completed rotation: values are the row's
// COMPLETE live set afterwards (fresh access token, fresh refresh token, and
// the client secret that still authenticates it), and everything the row held
// before starts its retirement grace window. Passing an incomplete set would
// start the clock on a live secret, so every caller lists all of them.
func (s *Service) noteRotatedSecrets(scope string, values ...string) {
	s.observe(scope, true, values...)
}

func (s *Service) observe(scope string, rotated bool, values ...string) {
	if s.observeSecret == nil {
		return
	}
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			kept = append(kept, v)
		}
	}
	// A rotation with nothing to register still has to be reported: it is what
	// retires the previous generation.
	if len(kept) == 0 && !rotated {
		return
	}
	s.observeSecret(scope, rotated, kept...)
}

// Enabled reports whether the feature can operate (encryption key + base URL).
func (s *Service) Enabled() bool {
	return s.store != nil && s.store.RemoteMCPEncryptionEnabled() && strings.TrimSpace(s.cfg.PublicBaseURL) != ""
}

// RedirectURI is the stable, byte-identical callback used everywhere.
func (s *Service) RedirectURI() string {
	return strings.TrimRight(s.cfg.PublicBaseURL, "/") + s.cfg.CallbackPath
}

// AddServerInput is the request to register a new remote MCP server.
type AddServerInput struct {
	Email string
	Name  string
	URL   string
	// Account is the seat label for a further login under an existing Name
	// (#988): "work", "personal". Empty adds (or is) the unlabeled seat.
	Account string
	// AuthMode is "open" for servers that require no Authorization header, or
	// "api_key" for servers authenticated by a static vendor key. Empty and
	// "oauth" retain the discovery + authorization flow.
	AuthMode string
	// Optional manual client credentials for an authorization server without DCR.
	ClientID     string
	ClientSecret string
	// api_key mode only: the key itself (sealed at rest, never echoed back) and
	// the header NAME to send it under ("" = Authorization: Bearer <key>).
	APIKey       string
	APIKeyHeader string
	// APIKeyQuery is the query-parameter NAME to send the key under, for
	// vendors that authenticate in the URL (Browserbase). Mutually exclusive
	// with APIKeyHeader; the transport attaches the key per-request so it
	// never lands in the stored URL.
	APIKeyQuery string
}

// AddServer validates + canonicalizes the URL and persists the server. OAuth
// adds walk the discovery chain + dynamic client registration (or use manual
// credentials) and land in status login_required — the login flow itself
// proves the connection works. open and api_key adds carry no login step, so
// they are validated NOW with a real MCP handshake (probeServer) and only
// land connected if the server answers; a wrong key or mistyped tenant URL
// fails the add with an actionable error instead of surfacing mid-run. The
// returned tool count is >= 0 for probed adds and -1 when no probe ran.
func (s *Service) AddServer(ctx context.Context, in AddServerInput) (*store.RemoteMCPServer, int, error) {
	if !s.Enabled() {
		return nil, -1, ErrDisabled
	}
	canonURL, err := mcpoauth.ValidateServerURL(in.URL, s.cfg.AllowInsecureHTTP)
	if err != nil {
		return nil, -1, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, -1, errors.New("a name is required")
	}
	// Validate the seat label up front: an OAuth add registers a client at
	// the vendor before the row exists, and a label rejected only at insert
	// would leave that registration dangling.
	if _, err := store.CanonicalRemoteMCPAccount(in.Account); err != nil {
		return nil, -1, err
	}
	if in.AuthMode == store.RemoteMCPAuthOpen {
		toolCount, perr := s.probeServer(ctx, canonURL, "", "", "")
		if perr != nil {
			return nil, -1, fmt.Errorf("could not connect to the MCP server: %w", perr)
		}
		server, cerr := s.store.CreateRemoteMCPServer(ctx, store.RemoteMCPServerInput{
			UserEmail: in.Email, Name: name, Account: in.Account, URL: canonURL,
			Transport: store.RemoteMCPTransportStreamableHTTP,
			Status:    store.RemoteMCPStatusConnected,
			AuthKind:  store.RemoteMCPAuthOpen,
		})
		return server, toolCount, cerr
	}
	if in.AuthMode == store.RemoteMCPAuthAPIKey {
		header, verr := validateAPIKeyAuth(in.APIKey, in.APIKeyHeader)
		if verr != nil {
			return nil, -1, verr
		}
		query := strings.TrimSpace(in.APIKeyQuery)
		if query != "" {
			if header != "" {
				return nil, -1, errors.New("an API key is sent under a header OR a query parameter, not both")
			}
			if !mcp.QueryParamNameOK(query) {
				return nil, -1, fmt.Errorf("invalid API key query-parameter name %q", query)
			}
		}
		// Control-plane acquisition (#1274): the add-time probe sends this key
		// upstream and its failure text is returned to the caller (and logged),
		// so the key is registered for literal redaction before the probe runs.
		// No scope: an api_key does not rotate on a clock, and one manual
		// rotation per user cannot grow the literal set the way token refresh
		// did. There is no row yet either.
		s.noteSecrets(in.APIKey)
		toolCount, perr := s.probeServer(ctx, canonURL, header, query, in.APIKey)
		if perr != nil {
			return nil, -1, fmt.Errorf("the server did not accept this API key — check the key and try again: %w", perr)
		}
		server, cerr := s.store.CreateRemoteMCPServer(ctx, store.RemoteMCPServerInput{
			UserEmail: in.Email, Name: name, Account: in.Account, URL: canonURL,
			Transport:    store.RemoteMCPTransportStreamableHTTP,
			Status:       store.RemoteMCPStatusConnected,
			AuthKind:     store.RemoteMCPAuthAPIKey,
			APIKey:       in.APIKey,
			APIKeyHeader: header,
			APIKeyQuery:  query,
		})
		return server, toolCount, cerr
	}
	if in.AuthMode != "" && in.AuthMode != store.RemoteMCPAuthOAuth {
		return nil, -1, fmt.Errorf("unsupported remote MCP auth mode %q", in.AuthMode)
	}

	disco, err := mcpoauth.Discover(ctx, s.httpClient, canonURL)
	if err != nil {
		return nil, -1, fmt.Errorf("discover authorization server: %w", err)
	}

	scopes := strings.Join(disco.PRM.ScopesSupported, " ")
	if scopes == "" {
		scopes = strings.Join(disco.AS.ScopesSupported, " ")
	}

	clientID := strings.TrimSpace(in.ClientID)
	clientSecret := in.ClientSecret
	regToken := ""
	if clientID == "" {
		if disco.AS.RegistrationEndpoint == "" {
			return nil, -1, ErrManualClientRequired
		}
		reg, rerr := mcpoauth.Register(ctx, s.httpClient, disco.AS.RegistrationEndpoint, s.cfg.ClientName, s.RedirectURI(), scopes)
		if rerr != nil {
			return nil, -1, fmt.Errorf("dynamic client registration: %w", rerr)
		}
		clientID = reg.ClientID
		clientSecret = reg.ClientSecret
		regToken = reg.RegistrationAccessToken
	}
	// Control-plane acquisition (#1274): a dynamic registration just minted a
	// client secret + registration access token, and a manual add supplied one
	// — credentials this process now holds and whose later use (the login
	// exchange, a registration update) can quote them in an error. No scope:
	// these live for the client REGISTRATION's lifetime, not a token's, and
	// there is no server row to scope them to until the insert below.
	s.noteSecrets(clientSecret, regToken)

	server, err := s.store.CreateRemoteMCPServer(ctx, store.RemoteMCPServerInput{
		UserEmail:             in.Email,
		Name:                  name,
		Account:               in.Account,
		URL:                   disco.Resource,
		Transport:             store.RemoteMCPTransportStreamableHTTP,
		Issuer:                disco.AS.Issuer,
		AuthorizationEndpoint: disco.AS.AuthorizationEndpoint,
		TokenEndpoint:         disco.AS.TokenEndpoint,
		RegistrationEndpoint:  disco.AS.RegistrationEndpoint,
		RevocationEndpoint:    disco.AS.RevocationEndpoint,
		Scopes:                scopes,
		AuthMethods:           strings.Join(disco.AS.TokenEndpointAuthMethodsSupported, " "),
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		RegistrationToken:     regToken,
	})
	return server, -1, err
}

// probeServer performs a real MCP handshake — initialize + tools/list, over
// the SSRF-safe client — against url with the given credential, and returns
// the server's tool count. It is the add/rotate-time validation for
// connections that have no OAuth login step: without it a wrong key or a
// mistyped tenant URL would be stored as "connected" and only fail mid-run,
// where the error is far from the person who can fix it. The ephemeral client
// is closed before returning; nothing is registered.
func (s *Service) probeServer(ctx context.Context, url, headerName, queryName, credential string) (int, error) {
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()
	opts := mcp.HTTPServerOptions{HTTPClient: s.httpClient}
	switch {
	case credential != "" && queryName != "":
		// Query-authenticated vendor: the transport attaches the key so the
		// probed URL (which lands in error strings) never carries it.
		opts.HTTPClient = mcp.WithQueryParam(s.httpClient, queryName, credential)
	case credential != "":
		header, value := "Authorization", "Bearer "+credential
		if headerName != "" {
			header, value = headerName, credential
		}
		opts.Headers = map[string]string{header: value}
	}
	pctx, cancel := context.WithTimeout(ctx, s.cfg.HTTPTimeout)
	defer cancel()
	if err := client.AddHTTPServerWithOptions(pctx, "verify", url, opts); err != nil {
		return 0, err
	}
	return len(client.GetAllTools()), nil
}

// validateAPIKeyAuth vets a user-supplied API key + header name before either
// is persisted. The header name must be an RFC 7230 token and must not collide
// with headers the MCP transport itself owns; the key must be a single line of
// visible ASCII (Go's transport would reject anything else at send time, but a
// clear error here beats a mid-run failure). Returns the trimmed header name.
func validateAPIKeyAuth(key, header string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errors.New("an API key is required")
	}
	for _, r := range key {
		if r < 0x20 || r > 0x7e {
			return "", errors.New("the API key contains characters that cannot be sent in an HTTP header")
		}
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return "", nil // default: Authorization: Bearer <key>
	}
	for _, r := range header {
		headerOK := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !headerOK {
			return "", fmt.Errorf("invalid API key header name %q", header)
		}
	}
	switch strings.ToLower(header) {
	case "host", "content-length", "content-type", "transfer-encoding", "connection", "mcp-session-id", "accept":
		return "", fmt.Errorf("header %q is reserved by the MCP transport", header)
	}
	return header, nil
}

// SetAPIKey replaces an api_key connection's stored key (rotation / fixing a
// mistyped key) and marks it connected again. Owner-scoped. The new key is
// validated with a real MCP handshake before it replaces the old one — a
// rejected key leaves the stored key untouched — and the probed tool count is
// returned for the UI's confirmation notice.
func (s *Service) SetAPIKey(ctx context.Context, email, serverID, apiKey string) (int, error) {
	if !s.Enabled() {
		return 0, ErrDisabled
	}
	if _, err := validateAPIKeyAuth(apiKey, ""); err != nil {
		return 0, err
	}
	server, err := s.store.GetRemoteMCPServer(ctx, email, serverID)
	if err != nil {
		return 0, err
	}
	if server.AuthKind != store.RemoteMCPAuthAPIKey {
		return 0, errors.New("this connection does not use an API key")
	}
	// Same registration as the add-time probe (#1274): the rotated key rides
	// this request and the wrapped failure reaches the caller.
	s.noteSecrets(apiKey)
	toolCount, err := s.probeServer(ctx, server.URL, server.APIKeyHeader, server.APIKeyQuery, apiKey)
	if err != nil {
		return 0, fmt.Errorf("the server did not accept this API key — the previous key is unchanged: %w", err)
	}
	return toolCount, s.store.SetRemoteMCPAPIKey(ctx, email, serverID, apiKey)
}

// SetDefaultSeat makes serverID the default seat among the caller's seats of
// the same connection name (#988). Owner-scoped.
func (s *Service) SetDefaultSeat(ctx context.Context, email, serverID string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	return s.store.SetRemoteMCPDefaultSeat(ctx, email, serverID)
}

// RenameSeat changes a seat's public account label (#988). Owner-scoped; the
// credential is untouched.
func (s *Service) RenameSeat(ctx context.Context, email, serverID, label string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	return s.store.RenameRemoteMCPAccount(ctx, email, serverID, label)
}

// Authorize starts the OAuth flow for a server and returns the authorization URL
// the browser must visit. The PKCE verifier + CSRF state are stored server-side,
// bound to this user, single-use.
func (s *Service) Authorize(ctx context.Context, email, serverID string) (string, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	server, err := s.store.GetRemoteMCPServer(ctx, email, serverID)
	if err != nil {
		return "", err
	}
	clientSecret, _, err := s.store.LoadServerSecrets(ctx, server)
	if err != nil {
		return "", err
	}
	verifier, challenge, err := mcpoauth.GeneratePKCE()
	if err != nil {
		return "", err
	}
	state, err := mcpoauth.GenerateState()
	if err != nil {
		return "", err
	}
	if err := s.store.BeginOAuthFlow(ctx, state, server.ID, server.UserEmail, verifier, s.cfg.FlowTTL); err != nil {
		return "", err
	}
	// Control-plane acquisition (#1274): the client secret is unsealed here and
	// will authenticate the exchange this flow ends in. Registering it now means
	// a store or URL-building failure in between cannot quote it unscrubbed.
	s.noteScopedSecrets(secretScope(server.ID), clientSecret)
	fc := s.flowConfig(server, clientSecret)
	return fc.AuthCodeURL(state, challenge), nil
}

// Complete finishes the OAuth flow: it consumes the single-use state, enforces
// that the completing user matches the initiator, exchanges the code for tokens
// (carrying the PKCE verifier + resource indicator), and stores them. Returns
// the now-connected server.
func (s *Service) Complete(ctx context.Context, email, state, code string) (*store.RemoteMCPServer, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	flow, err := s.store.ConsumeOAuthFlow(ctx, state)
	if err != nil {
		return nil, err
	}
	// CSRF / account-linking defense: the browser session completing the flow
	// MUST be the same user that initiated it.
	if !strings.EqualFold(flow.UserEmail, strings.ToLower(strings.TrimSpace(email))) {
		return nil, fmt.Errorf("oauth callback user mismatch")
	}
	server, err := s.store.GetRemoteMCPServer(ctx, flow.UserEmail, flow.ServerID)
	if err != nil {
		return nil, err
	}
	clientSecret, _, err := s.store.LoadServerSecrets(ctx, server)
	if err != nil {
		return nil, err
	}
	// Control-plane acquisition (#1274): the client secret and the single-use
	// authorization code both ride the exchange request, whose failure text is
	// returned to the caller and logged, so they are registered BEFORE it is
	// sent. Both join this row's current generation; the successful exchange
	// below then supersedes them, which is what retires the spent code.
	scope := secretScope(server.ID)
	s.noteScopedSecrets(scope, clientSecret, code)
	fc := s.flowConfig(server, clientSecret)
	tok, err := fc.Exchange(ctx, s.httpClient, code, flow.CodeVerifier)
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}
	// The row's first token generation. Same contract as refreshFunc: the whole
	// live set, so the client secret stays live across the swap.
	s.noteRotatedSecrets(scope, clientSecret, tok.AccessToken, tok.RefreshToken)
	if err := s.store.StoreOAuthTokens(ctx, server, store.RemoteMCPTokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiryUnix(tok),
	}); err != nil {
		return nil, err
	}
	return s.store.GetRemoteMCPServer(ctx, server.UserEmail, server.ID)
}

// ListServers returns a user's servers (no secrets).
func (s *Service) ListServers(ctx context.Context, email string) ([]store.RemoteMCPServer, error) {
	return s.store.ListRemoteMCPServers(ctx, email)
}

// Sharing (#443 follow-up) — thin store passthroughs so the HTTP layer keeps a
// single service seam. Ownership checks live in the store; the token never
// leaves the host regardless of who a server is shared with.

// ShareServer grants grantee (an email, or store.GranteeEveryone) use of the
// owner's server.
func (s *Service) ShareServer(ctx context.Context, ownerEmail, serverID, grantee string) error {
	return s.store.ShareRemoteMCPServer(ctx, ownerEmail, serverID, grantee)
}

// UnshareServer revokes a grant.
func (s *Service) UnshareServer(ctx context.Context, ownerEmail, serverID, grantee string) error {
	return s.store.UnshareRemoteMCPServer(ctx, ownerEmail, serverID, grantee)
}

// SharesByOwner returns every grant on the owner's servers, keyed by server id.
func (s *Service) SharesByOwner(ctx context.Context, ownerEmail string) (map[string][]string, error) {
	return s.store.ListRemoteMCPSharesByOwner(ctx, ownerEmail)
}

// SharedWithMe returns servers other users shared with email; each row's
// UserEmail is the owner (attribution).
func (s *Service) SharedWithMe(ctx context.Context, email string) ([]store.RemoteMCPServer, error) {
	return s.store.ListRemoteMCPServersSharedWith(ctx, email)
}

// Disconnect best-effort revokes the refresh token at the authorization server,
// then deletes the server (cascading its tokens + any in-flight flow).
func (s *Service) Disconnect(ctx context.Context, email, serverID string) error {
	server, err := s.store.GetRemoteMCPServer(ctx, email, serverID)
	if err != nil {
		return err
	}
	if server.RevocationEndpoint != "" {
		// Best effort: a revocation failure must not block the local delete.
		s.tryRevoke(ctx, server)
	}
	return s.store.DeleteRemoteMCPServer(ctx, email, serverID)
}

// SignOut ends the caller's authorization for an OAuth server without deleting
// the registration: best-effort token revocation at the AS, then the stored
// tokens are dropped and the server is marked needs_reauth so the UI offers
// Reconnect (client ID/secret survive). Disconnect remains the full delete.
func (s *Service) SignOut(ctx context.Context, email, serverID string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	server, err := s.store.GetRemoteMCPServer(ctx, email, serverID)
	if err != nil {
		return err
	}
	if server.AuthKind != "" && server.AuthKind != "oauth" {
		return fmt.Errorf("sign out applies to OAuth connections only (this one is %s)", server.AuthKind)
	}
	if server.RevocationEndpoint != "" {
		s.tryRevoke(ctx, server)
	}
	return s.store.ClearOAuthTokens(ctx, email, serverID)
}

// AcquireToken returns a valid bearer for server, refreshing under the store's
// row lock if needed. It is the SINGLE refresh path shared by chat turns and
// scheduled tasks. On a dead refresh token it returns store.ErrRemoteMCPNeedsReauth
// (the server is marked needs_reauth) so callers degrade gracefully.
func (s *Service) AcquireToken(ctx context.Context, server *store.RemoteMCPServer) (string, error) {
	clientSecret, _, err := s.store.LoadServerSecrets(ctx, server)
	if err != nil {
		return "", err
	}
	// The unsealed client secret rides the same token-endpoint request as the
	// refresh token (client authentication), so it is the same hazard class:
	// register it for literal redaction before that request can quote it in an
	// error (#1124 follow-up). Scoped to this row and NOT marked as a rotation:
	// it survives every token rotation, and each rotation re-lists it, so it
	// stays live in the scan set (#1274).
	scope := secretScope(server.ID)
	s.noteScopedSecrets(scope, clientSecret)
	fc := s.flowConfig(server, clientSecret)
	margin := int64(s.cfg.RefreshMargin / time.Second)
	bearer, err := s.store.EnsureFreshToken(ctx, server, margin, s.refreshFunc(scope, fc))
	if err == nil {
		// Runtime-acquired bearer: register it for literal redaction before it
		// is used on any request (#1124). A bearer the store returned unchanged
		// joins the row's current generation; one a refresh just minted is
		// already in it (refreshFunc rotated the generation), where this is a
		// no-op.
		s.noteScopedSecrets(scope, bearer)
	}
	return bearer, err
}

func (s *Service) refreshFunc(scope string, fc mcpoauth.FlowConfig) store.RefreshFunc {
	return func(ctx context.Context, current store.RemoteMCPTokens) (store.RefreshResult, error) {
		// The stored refresh token is about to ride a token-endpoint request
		// whose failure text could echo it; the ROTATED tokens below never
		// leave this closure (the fresh refresh token in particular is only
		// stored, not returned). Register all of them for literal redaction
		// at acquisition time (#1124).
		s.noteScopedSecrets(scope, current.RefreshToken)
		rctx, cancel := context.WithTimeout(ctx, s.cfg.RefreshTimeout)
		defer cancel()
		tok, err := fc.Refresh(rctx, s.httpClient, current.RefreshToken)
		if err != nil {
			// Terminal failures condemn the stored grant or client registration:
			// re-issuing the same refresh can never succeed, so hand the
			// connection back to the user as needs-reauth (reconnecting re-runs
			// registration through the normal connect flow) instead of returning
			// the same opaque error on every run forever. Everything else —
			// network, 5xx — stays transient and is retried on the next call.
			if mcpoauth.IsTerminalRefreshError(err) {
				return store.RefreshResult{
					NeedReauth:   true,
					ReauthDetail: mcpoauth.ReauthDetail(err),
				}, nil
			}
			return store.RefreshResult{}, err
		}
		// The rotation SUCCEEDED, so this is the row's new generation: the fresh
		// pair plus the client secret that still authenticates it. Everything
		// the row held before — the access token this one replaces, the refresh
		// token this rotation consumed — enters its grace window and is dropped
		// after it (#1274). Listing the client secret here is what keeps it
		// live across the swap.
		s.noteRotatedSecrets(scope, fc.ClientSecret, tok.AccessToken, tok.RefreshToken)
		return store.RefreshResult{Tokens: store.RemoteMCPTokens{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    expiryUnix(tok),
		}}, nil
	}
}

func (s *Service) tryRevoke(ctx context.Context, server *store.RemoteMCPServer) {
	rctx, cancel := context.WithTimeout(ctx, s.cfg.RefreshTimeout)
	defer cancel()
	// Best effort: failures are swallowed because the local delete is
	// authoritative for the user. Revoke the refresh token so it can't be reused.
	clientSecret, _, err := s.store.LoadServerSecrets(ctx, server)
	if err != nil {
		return
	}
	tokens, err := s.store.GetOAuthTokens(ctx, server)
	if err != nil || tokens.RefreshToken == "" {
		return
	}
	// Same registration as AcquireToken/refreshFunc: both secrets ride the
	// revocation request and could be echoed by its failure text. A join, not a
	// rotation — revocation mints nothing.
	s.noteScopedSecrets(secretScope(server.ID), clientSecret, tokens.RefreshToken)
	_ = mcpoauth.RevokeToken(rctx, s.httpClient, server.RevocationEndpoint, server.ClientID, clientSecret, tokens.RefreshToken)
}

func (s *Service) flowConfig(server *store.RemoteMCPServer, clientSecret string) mcpoauth.FlowConfig {
	return mcpoauth.FlowConfig{
		AuthorizationEndpoint: server.AuthorizationEndpoint,
		TokenEndpoint:         server.TokenEndpoint,
		ClientID:              server.ClientID,
		ClientSecret:          clientSecret,
		RedirectURI:           s.RedirectURI(),
		Scopes:                splitFields(server.Scopes),
		Resource:              server.URL,
		AuthMethods:           splitFields(server.AuthMethods),
	}
}

func splitFields(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

func expiryUnix(tok *mcpoauth.Token) int64 {
	if tok.Expiry.IsZero() {
		return 0
	}
	return tok.Expiry.Unix()
}
