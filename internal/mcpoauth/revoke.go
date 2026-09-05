package mcpoauth

import (
	"context"
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
// the same basicAuthAllowed decision the exchange and refresh use picks Basic
// vs. client_secret_post here — a mismatch is a 401 the AS answers with the
// token still live.
func RevokeToken(ctx context.Context, httpClient *http.Client, revocationEndpoint, clientID, clientSecret, token string, authMethods []string) error {
	if revocationEndpoint == "" || token == "" {
		return nil
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", "refresh_token")

	useBasic := clientSecret != "" && basicAuthAllowed(authMethods)
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
