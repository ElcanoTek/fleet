package mcpoauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRevokeToken(t *testing.T) {
	t.Run("early return", func(t *testing.T) {
		if err := RevokeToken(context.Background(), http.DefaultClient, "", "client_id", "client_secret", "token", nil); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
		if err := RevokeToken(context.Background(), http.DefaultClient, "https://example.com/revoke", "client_id", "client_secret", "", nil); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	})

	t.Run("with client secret", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST method, got %s", r.Method)
			}
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("expected application/x-www-form-urlencoded, got %s", r.Header.Get("Content-Type"))
			}

			user, pass, ok := r.BasicAuth()
			if !ok {
				t.Errorf("expected Basic Auth")
			}
			if user != "client_id" || pass != "client_secret" {
				t.Errorf("expected client_id/client_secret basic auth, got %s/%s", user, pass)
			}

			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("token") != "test_token" {
				t.Errorf("expected token=test_token, got %s", values.Get("token"))
			}
			if values.Get("token_type_hint") != "refresh_token" {
				t.Errorf("expected token_type_hint=refresh_token, got %s", values.Get("token_type_hint"))
			}
			if values.Get("client_id") != "" {
				t.Errorf("expected empty client_id in body, got %s", values.Get("client_id"))
			}

			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		if err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "client_secret", "test_token", nil); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	})

	t.Run("without client secret", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST method, got %s", r.Method)
			}
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("expected application/x-www-form-urlencoded, got %s", r.Header.Get("Content-Type"))
			}

			_, _, ok := r.BasicAuth()
			if ok {
				t.Errorf("expected no Basic Auth")
			}

			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("token") != "test_token" {
				t.Errorf("expected token=test_token, got %s", values.Get("token"))
			}
			if values.Get("token_type_hint") != "refresh_token" {
				t.Errorf("expected token_type_hint=refresh_token, got %s", values.Get("token_type_hint"))
			}
			if values.Get("client_id") != "client_id" {
				t.Errorf("expected client_id=client_id in body, got %s", values.Get("client_id"))
			}

			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		if err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "", "test_token", nil); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	})

	// The regression this signature exists for: an AS that advertises ONLY
	// client_secret_post must be authenticated in the body, not with Basic.
	// Hard-coded Basic made the revocation 401 while the local record was
	// deleted anyway, so a user who clicked Disconnect kept a live refresh
	// token at the authorization server.
	t.Run("client_secret_post-only AS is authenticated in the body", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, ok := r.BasicAuth(); ok {
				t.Error("Basic auth sent to a client_secret_post-only AS")
			}
			body, _ := io.ReadAll(r.Body)
			values, err := url.ParseQuery(string(body))
			if err != nil {
				t.Fatalf("parse body: %v", err)
			}
			if values.Get("client_id") != "client_id" {
				t.Errorf("client_id in body = %q, want %q", values.Get("client_id"), "client_id")
			}
			if values.Get("client_secret") != "client_secret" {
				t.Errorf("client_secret in body = %q, want %q", values.Get("client_secret"), "client_secret")
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		if err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "client_secret", "test_token",
			[]string{"client_secret_post"}); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	})

	// An AS that advertises both keeps Basic — the same choice the token
	// endpoint makes, so exchange/refresh/revoke never disagree.
	t.Run("an AS advertising client_secret_basic keeps Basic", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok {
				t.Fatal("expected Basic auth")
			}
			if user != "client_id" || pass != "client_secret" {
				t.Errorf("basic auth = %s/%s, want client_id/client_secret", user, pass)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		if err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "client_secret", "test_token",
			[]string{"client_secret_post", "client_secret_basic"}); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	})

	t.Run("non-2xx is reported, not swallowed", func(t *testing.T) {
		// RFC 7009 §2.2.1 refusals arrive as an RFC 6749 §5.2 body. Revocation is
		// best-effort, but "the AS refused" must not read as "revoked".
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		}))
		defer ts.Close()

		err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "client_secret", "test_token", nil)
		if err == nil {
			t.Fatal("expected an error for a 401 revocation response, got nil")
		}
		var oe *OAuthError
		if !errors.As(err, &oe) {
			t.Fatalf("expected an *OAuthError, got %T: %v", err, err)
		}
		if oe.Code != "invalid_client" {
			t.Errorf("Code = %q, want %q", oe.Code, "invalid_client")
		}
		if oe.HTTPStatus != http.StatusUnauthorized {
			t.Errorf("HTTPStatus = %d, want %d", oe.HTTPStatus, http.StatusUnauthorized)
		}
	})

	t.Run("an unknown token still counts as revoked", func(t *testing.T) {
		// RFC 7009 §2.2: the AS answers 200 when the token was already invalid.
		// That is a successful outcome, not a failure to report.
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		if err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "", "stale_token", nil); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	})

	t.Run("http error", func(t *testing.T) {
		// Use an invalid URL to force an error from http.NewRequestWithContext or httpClient.Do
		err := RevokeToken(context.Background(), http.DefaultClient, "://invalid-url", "client_id", "", "test_token", nil)
		if err == nil {
			t.Errorf("expected error, got nil")
		}

		// Use a closed test server to force an error from httpClient.Do
		ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		ts.Close()
		err = RevokeToken(context.Background(), http.DefaultClient, ts.URL, "client_id", "", "test_token", nil)
		if err == nil {
			t.Errorf("expected error, got nil")
		}
	})
}
