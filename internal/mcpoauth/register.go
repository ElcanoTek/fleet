package mcpoauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ClientRegistration is the result of RFC 7591 Dynamic Client Registration: the
// credentials this fleet deployment uses to speak OAuth to one authorization
// server. It is registered ONCE per (issuer, deployment) and shared across all
// users — DCR identifies the client APP, not the user; the per-user part is the
// token. A returned client_secret and the RFC 7592 registration_access_token are
// secrets and are encrypted at rest by the caller.
type ClientRegistration struct {
	ClientID                string `json:"client_id"`
	ClientSecret            string `json:"client_secret"`
	ClientSecretExpiresAt   int64  `json:"client_secret_expires_at"`
	RegistrationAccessToken string `json:"registration_access_token"`
	RegistrationClientURI   string `json:"registration_client_uri"`
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
// set to request. httpClient MUST be the SSRF-safe client in production.
func Register(ctx context.Context, httpClient *http.Client, registrationEndpoint, clientName, redirectURI, scope string) (*ClientRegistration, error) {
	if registrationEndpoint == "" {
		return nil, fmt.Errorf("authorization server does not advertise a registration_endpoint (dynamic client registration unsupported)")
	}
	reqBody := clientRegistrationRequest{
		ClientName:   clientName,
		RedirectURIs: []string{redirectURI},
		GrantTypes:   []string{"authorization_code", "refresh_token"},
		// We are a public client (PKCE-protected) by default — no client secret
		// to keep. An AS that insists on a confidential client will return one,
		// which we store encrypted and use via client_secret_basic.
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
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
		return nil, fmt.Errorf("dynamic client registration failed: status %d", resp.StatusCode)
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
