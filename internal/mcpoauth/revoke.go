package mcpoauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// RevokeToken makes a best-effort RFC 7009 token revocation request. A failure
// is returned but callers typically ignore it — the local record is deleted
// regardless, and an unreachable/again-non-supporting AS shouldn't block the
// user from disconnecting. token is the refresh (or access) token to revoke.
func RevokeToken(ctx context.Context, httpClient *http.Client, revocationEndpoint, clientID, clientSecret, token string) error {
	if revocationEndpoint == "" || token == "" {
		return nil
	}
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", "refresh_token")

	useBasic := clientSecret != ""
	if !useBasic {
		form.Set("client_id", clientID)
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
	_ = resp.Body.Close()
	return nil
}
