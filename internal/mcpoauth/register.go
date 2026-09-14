package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ClientRegistration is the result of RFC 7591 Dynamic Client Registration: the
// credentials this fleet deployment uses to speak OAuth to one authorization
// server. It is registered ONCE per (issuer, deployment) and shared across all
// users — DCR identifies the client APP, not the user; the per-user part is the
// token. A returned client_secret and the RFC 7592 registration_access_token are
// secrets and are encrypted at rest by the caller.
type ClientRegistration struct {
	ClientID              string `json:"client_id"`
	ClientSecret          string `json:"client_secret"`
	ClientSecretExpiresAt int64  `json:"client_secret_expires_at"`
	// TokenEndpointAuthMethod is the method the server says this client
	// ACTUALLY has, which RFC 7591 §3.2.1 lets it substitute for the one we
	// asked for. It is narrower and more authoritative than the authorization
	// server's advertised list: a server may advertise both client_secret_basic
	// and client_secret_post and still register this client as post-only, and
	// picking Basic off the advertised list would then 401 every exchange and
	// revocation. Empty when the server does not echo it.
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	RegistrationAccessToken string `json:"registration_access_token"`
	RegistrationClientURI   string `json:"registration_client_uri"`
}

// EffectiveAuthMethods is the token-endpoint authentication method list to
// store for a dynamically registered client: the single method the server
// echoed back, when it named a confidential one, else the authorization
// server's advertised list.
//
// A "none" echo does NOT narrow the list. fleet asks to be a public client
// first, and of the servers measured in the #1006 audit one echoed "none" and
// returned a client_secret anyway; storing "none" there would drop the secret
// out of the token request. The advertised list is the better guide in that
// case, and PublicClientAllowed already decides separately whether a
// secretless client is permitted at all.
func (r *ClientRegistration) EffectiveAuthMethods(advertised []string) []string {
	m := strings.TrimSpace(r.TokenEndpointAuthMethod)
	if strings.EqualFold(m, "client_secret_basic") || strings.EqualFold(m, "client_secret_post") {
		return []string{strings.ToLower(m)}
	}
	return advertised
}

// clientRegistrationRequest is the RFC 7591 registration payload. We register a
// public-or-confidential client for the authorization-code grant with our fixed
// redirect URI.
type clientRegistrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

// Register performs RFC 7591 Dynamic Client Registration against
// registrationEndpoint. clientName is a human label shown on consent screens;
// redirectURI is the single, byte-stable callback; scope is the space-delimited
// set to request; authMethodsSupported is the authorization server's
// token_endpoint_auth_methods_supported (nil when it advertised none).
// httpClient MUST be the SSRF-safe client in production.
//
// fleet asks to be a public client (token_endpoint_auth_method "none", PKCE
// protected) first, whatever the metadata says: of the 22 official catalog
// servers whose metadata lists no "none", the two met live (Uptime Robot,
// Cartesia) both accepted the request — one returning a secret fleet then
// stores and uses, one registering a public client outright (#1006 audit). An
// authorization server that instead REFUSES the method (RFC 7591 §3.2.2
// invalid_client_metadata) gets one retry asking for a confidential method it
// does list, client_secret_basic before client_secret_post, so the vendor's
// stated policy is honored without giving up the public client where it is
// allowed.
func Register(ctx context.Context, httpClient *http.Client, registrationEndpoint, clientName, redirectURI, scope string, authMethodsSupported []string) (*ClientRegistration, error) {
	if registrationEndpoint == "" {
		return nil, fmt.Errorf("authorization server does not advertise a registration_endpoint (dynamic client registration unsupported)")
	}
	reg, err := registerWithMethod(ctx, httpClient, registrationEndpoint, clientName, redirectURI, scope, "none")
	if err == nil {
		return reg, nil
	}
	var rej *registrationRejectedError
	if !errors.As(err, &rej) || !rej.aboutAuthMethod() {
		return nil, err
	}
	fallback := confidentialMethodFor(authMethodsSupported)
	if fallback == "" {
		return nil, err
	}
	reg, ferr := registerWithMethod(ctx, httpClient, registrationEndpoint, clientName, redirectURI, scope, fallback)
	if ferr != nil {
		return nil, fmt.Errorf("%w; retried as %s: %w", err, fallback, ferr)
	}
	if reg.ClientSecret == "" {
		// We asked to be a confidential client only because this server had
		// just refused a public one, so a registration that comes back with no
		// client_secret cannot authenticate at the token endpoint: storing it
		// would send the user through a consent screen whose code exchange is
		// bound to fail. Fail here instead, where the reason is readable.
		return nil, fmt.Errorf("%w; retried as %s and the server registered client %q with no client_secret, which cannot authenticate at the token endpoint", err, fallback, reg.ClientID)
	}
	return reg, nil
}

// registrationRejectedError is a 4xx registration response with the RFC 7591 §3.2.2
// error body, kept typed so Register can tell "the server refused the auth
// method" from "the endpoint is broken".
type registrationRejectedError struct {
	// json:"-": the status is the HTTP one, and encoding/json matches field
	// names case-insensitively — without this, a body carrying its own
	// "status" would overwrite it and could turn a 500 into a retryable 400.
	Status      int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *registrationRejectedError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("dynamic client registration failed: status %d", e.Status)
	}
	return fmt.Sprintf("dynamic client registration failed: status %d, %s: %s", e.Status, e.Code, e.Description)
}

// aboutAuthMethod: RFC 7591 §3.2.2 names invalid_client_metadata for a
// rejected metadata value; a server that spells its objection out names the
// field or the value instead.
func (e *registrationRejectedError) aboutAuthMethod() bool {
	if e.Status != http.StatusBadRequest && e.Status != http.StatusUnprocessableEntity {
		return false
	}
	if strings.EqualFold(e.Code, "invalid_client_metadata") {
		return true
	}
	d := strings.ToLower(e.Description)
	return strings.Contains(d, "token_endpoint_auth_method") || strings.Contains(d, "auth_method") || strings.Contains(d, "public client")
}

// confidentialMethodFor picks the confidential client authentication fleet can
// perform from an authorization server's advertised list: client_secret_basic
// first (the RFC 6749 default), then client_secret_post. "" when the server
// lists neither — then there is nothing to retry with.
func confidentialMethodFor(methods []string) string {
	if len(methods) == 0 {
		// RFC 8414 §2 defines an omitted token_endpoint_auth_methods_supported
		// as exactly ["client_secret_basic"] — the default basicAuthAllowed and
		// PublicClientAllowed already apply at the token endpoint. A server
		// that publishes no list and then refuses `none` is asking for Basic,
		// so retry with it rather than giving up on a list that isn't there.
		return "client_secret_basic"
	}
	for _, want := range []string{"client_secret_basic", "client_secret_post"} {
		for _, m := range methods {
			if strings.EqualFold(strings.TrimSpace(m), want) {
				return want
			}
		}
	}
	return ""
}

func registerWithMethod(ctx context.Context, httpClient *http.Client, registrationEndpoint, clientName, redirectURI, scope, method string) (*ClientRegistration, error) {
	reqBody := clientRegistrationRequest{
		ClientName:              clientName,
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: method,
		Scope:                   scope,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dynamic client registration request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return nil, fmt.Errorf("read registration response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		rej := &registrationRejectedError{Status: resp.StatusCode}
		_ = json.Unmarshal(body, rej) // best effort: a non-JSON body leaves Code empty
		return nil, rej
	}
	var reg ClientRegistration
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, fmt.Errorf("decode registration response: %w", err)
	}
	if reg.ClientID == "" {
		return nil, fmt.Errorf("dynamic client registration returned no client_id")
	}
	return &reg, nil
}
