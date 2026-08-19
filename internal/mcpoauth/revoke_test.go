package mcpoauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRevokeToken(t *testing.T) {
	t.Run("early return", func(t *testing.T) {
		if err := RevokeToken(context.Background(), http.DefaultClient, "", "client_id", "client_secret", "token"); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
		if err := RevokeToken(context.Background(), http.DefaultClient, "https://example.com/revoke", "client_id", "client_secret", ""); err != nil {
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

		if err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "client_secret", "test_token"); err != nil {
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

		if err := RevokeToken(context.Background(), ts.Client(), ts.URL, "client_id", "", "test_token"); err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	})

	t.Run("http error", func(t *testing.T) {
		// Use an invalid URL to force an error from http.NewRequestWithContext or httpClient.Do
		err := RevokeToken(context.Background(), http.DefaultClient, "://invalid-url", "client_id", "", "test_token")
		if err == nil {
			t.Errorf("expected error, got nil")
		}

		// Use a closed test server to force an error from httpClient.Do
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		ts.Close()
		err = RevokeToken(context.Background(), http.DefaultClient, ts.URL, "client_id", "", "test_token")
		if err == nil {
			t.Errorf("expected error, got nil")
		}
	})
}
