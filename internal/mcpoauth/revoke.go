package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// RevokeToken makes a best-effort RFC 7009 token revocation request. Best-effort
// describes the caller's posture, not the report: a transport failure and a
// non-2xx refusal both come back as errors, so a caller that wants to log or
// retry can — the local record is deleted regardless, and an unreachable or
// non-supporting AS shouldn't block the user from disconnecting. token is the
// refresh (or access) token to revoke. authMethods is the AS's advertised
// token_endpoint_auth_methods_supported (nil when it advertised none): RFC 7009
// §2.1 has the client authenticate exactly as it does at the token endpoint, so
// the same basicAuthAllowed decision the exchange and refresh use picks the
// first attempt's form here.
//
// That advertised list is only a strong hint, though. RFC 8414 §2 defines a
// SEPARATE revocation_endpoint_auth_methods_supported, defaulting to the token
// endpoint's when omitted, and servers do sometimes accept a different form at
// each. Getting it wrong is silent and expensive: the AS answers 401, the
// caller deletes the local record anyway, and the user who clicked Disconnect
// keeps a live refresh token. So a confidential client that is refused for
// AUTHENTICATION retries once with the other form. Revocation is idempotent
// (RFC 7009 §2.2 makes an unknown token a 200), which is what makes a second
// attempt safe, and it costs one request only on a path that was going to fail.
func RevokeToken(ctx context.Context, httpClient *http.Client, revocationEndpoint, clientID, clientSecret, token string, authMethods []string) error {
	if revocationEndpoint == "" || token == "" {
		return nil
	}
	useBasic := clientSecret != "" && basicAuthAllowed(authMethods)
	err := revokeOnce(ctx, httpClient, revocationEndpoint, clientID, clientSecret, token, useBasic)
	// Only a confidential client has two forms to choose between, and only an
	// authentication refusal is evidence the choice was the wrong one — a 400
	// invalid_request or a 503 says nothing about the auth form, so retrying
	// those would just double the noise.
	if err != nil && clientSecret != "" && isAuthRefusal(err) {
		if retryErr := revokeOnce(ctx, httpClient, revocationEndpoint, clientID, clientSecret, token, !useBasic); retryErr == nil {
			return nil
		}
	}
	return err
}

// isAuthRefusal reports whether the AS rejected the request's CLIENT
// AUTHENTICATION, as opposed to refusing the request on its merits. Both the
// status (401/403) and RFC 6749 §5.2's invalid_client code count: some servers
// answer 400 with invalid_client rather than 401.
func isAuthRefusal(err error) bool {
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		return false
	}
	return oauthErr.HTTPStatus == http.StatusUnauthorized ||
		oauthErr.HTTPStatus == http.StatusForbidden ||
		oauthErr.Code == "invalid_client"
}

// revokeOnce is one RFC 7009 revocation attempt in the given client-auth form.
func revokeOnce(ctx context.Context, httpClient *http.Client, revocationEndpoint, clientID, clientSecret, token string, useBasic bool) error {
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", "refresh_token")

	if !useBasic {
		// Public client, or client_secret_post: client_id (and secret) in body.
		form.Set("client_id", clientID)
		if clientSecret != "" {
			form.Set("client_secret", clientSecret)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if useBasic {
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Read the body before reporting on the status: RFC 6749 §5.2 carries the
	// reason there, and draining it lets the connection be reused.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return fmt.Errorf("read revocation response: %w", err)
	}
	// RFC 7009 §2.2: 200 means revoked — and also means "you handed me a token I
	// don't know", which is equally fine here. Anything outside 2xx is a real
	// refusal (§2.2.1), so surface it as the same OAuthError the token endpoint
	// produces instead of reporting success. Callers still decide what to do:
	// revocation is best-effort and the local record goes away regardless.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseTokenError(raw, resp.StatusCode)
	}
	return nil
}
