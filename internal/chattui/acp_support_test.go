package chattui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The pieces `fleet acp` (internal/acp) relies on: the public web URL for
// approval deep links, the server-side Stop call, the client label, and a
// typed status error so an auth failure is distinguishable without parsing text.

func TestResolvePublicURL(t *testing.T) {
	base := map[string]string{"FLEET_SERVER_TOKEN": "tok", "FLEET_USER_EMAIL": "a@b.c"}
	t.Run("env, base URL preferred, trailing slash trimmed", func(t *testing.T) {
		env := map[string]string{"FLEET_PUBLIC_BASE_URL": "https://fleet.example.com/", "FLEET_PUBLIC_URL": "https://other"}
		for k, v := range base {
			env[k] = v
		}
		cfg, err := Resolve(Flags{}, envMap(env), noFile, noEnvFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PublicURL != "https://fleet.example.com" {
			t.Errorf("PublicURL = %q", cfg.PublicURL)
		}
	})
	t.Run("from the server env file even when token and addr are already set", func(t *testing.T) {
		cfg, err := Resolve(Flags{EnvFile: "/etc/fleet/fleet.env"}, envMap(base), noFile,
			envFileFrom(map[string]map[string]string{"/etc/fleet/fleet.env": {"FLEET_PUBLIC_URL": "https://box.example.com"}}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PublicURL != "https://box.example.com" {
			t.Errorf("PublicURL = %q", cfg.PublicURL)
		}
	})
	t.Run("never from a different deployment's env file", func(t *testing.T) {
		// The token and address come from .env.local; /etc/fleet/fleet.env
		// belongs to another deployment and must not supply the deep link.
		cfg, err := Resolve(Flags{Email: "a@b.c"}, envMap(map[string]string{}), noFile, envFileFrom(map[string]map[string]string{
			".env.local":           {"FLEET_SERVER_TOKEN": "tok", "FLEET_SERVER_ADDR": "127.0.0.1:9000"},
			"/etc/fleet/fleet.env": {"FLEET_SERVER_TOKEN": "other", "FLEET_PUBLIC_URL": "https://other.example.com"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Token != "tok" || cfg.PublicURL != "" {
			t.Errorf("token=%q PublicURL=%q, want tok and no public URL", cfg.Token, cfg.PublicURL)
		}
	})
	t.Run("from the file that supplied the server config", func(t *testing.T) {
		cfg, err := Resolve(Flags{Email: "a@b.c"}, envMap(map[string]string{}), noFile, envFileFrom(map[string]map[string]string{
			"/etc/fleet/fleet.env": {"FLEET_SERVER_TOKEN": "tok", "FLEET_PUBLIC_BASE_URL": "https://box.example.com"},
		}))
		if err != nil || cfg.PublicURL != "https://box.example.com" {
			t.Errorf("PublicURL = %q, err %v", cfg.PublicURL, err)
		}
	})
	t.Run("token from env and no pinned file: not probed", func(t *testing.T) {
		cfg, err := Resolve(Flags{}, envMap(base), noFile, envFileFrom(map[string]map[string]string{
			"/etc/fleet/fleet.env": {"FLEET_PUBLIC_URL": "https://maybe-other.example.com"},
		}))
		if err != nil || cfg.PublicURL != "" {
			t.Errorf("PublicURL = %q, err %v", cfg.PublicURL, err)
		}
	})
	t.Run("unset is empty", func(t *testing.T) {
		cfg, err := Resolve(Flags{}, envMap(base), noFile, noEnvFile)
		if err != nil || cfg.PublicURL != "" {
			t.Errorf("PublicURL = %q, err %v", cfg.PublicURL, err)
		}
	})
}

func TestCancelStopsTheTurnServerSide(t *testing.T) {
	var gotPath, gotBody, gotClient, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.Method+" "+r.URL.Path, string(b)
		gotClient, gotToken = r.Header.Get("X-Fleet-Client"), r.Header.Get("X-Chat-Server-Token")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok", ClientName: "fleet-acp"})
	if err := c.Cancel("conv 1"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /conversations/conv 1/cancel" || gotBody != `{"scope":"turn"}` {
		t.Errorf("request = %s %s", gotPath, gotBody)
	}
	if gotClient != "fleet-acp" || gotToken != "tok" {
		t.Errorf("headers: client=%q token=%q", gotClient, gotToken)
	}
	if err := c.Cancel(""); err != nil {
		t.Errorf("no conversation yet must be a no-op, got %v", err)
	}
}

func TestStreamReturnsTypedStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Fleet-Client"); got != "fleet-chat" {
			t.Errorf("default client label = %q", got)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).Stream(context.Background(), "hi", "", func(Event) {})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("err = %#v, want *StatusError 403", err)
	}
	if se.Error() != "server rejected the request (403): check FLEET_SERVER_TOKEN matches the server" {
		t.Errorf("message changed: %q", se.Error())
	}
}
