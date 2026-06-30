package chattui

import (
	"errors"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	noFile := func(string) ([]byte, error) { return nil, errors.New("no file") }
	envMap := func(m map[string]string) getenv {
		return func(k string) string { return m[k] }
	}

	t.Run("flags win; server normalized", func(t *testing.T) {
		cfg, err := Resolve(
			Flags{Server: "http://host:9000/", Email: "Me@Example.com", Model: "x/y"},
			envMap(map[string]string{"FLEET_SERVER_TOKEN": "tok"}), noFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ServerURL != "http://host:9000" {
			t.Errorf("server = %q, want trimmed http://host:9000", cfg.ServerURL)
		}
		if cfg.Email != "me@example.com" {
			t.Errorf("email = %q, want lowercased", cfg.Email)
		}
		if cfg.Token != "tok" || cfg.Model != "x/y" {
			t.Errorf("token/model = %q/%q", cfg.Token, cfg.Model)
		}
	})

	t.Run("server default + FLEET_SERVER_ADDR", func(t *testing.T) {
		cfg, _ := Resolve(Flags{Email: "a@b.co"},
			envMap(map[string]string{"FLEET_SERVER_TOKEN": "t", "FLEET_SERVER_ADDR": "127.0.0.1:9999"}), noFile)
		if cfg.ServerURL != "http://127.0.0.1:9999" {
			t.Errorf("server = %q, want from FLEET_SERVER_ADDR", cfg.ServerURL)
		}
		cfg2, _ := Resolve(Flags{Email: "a@b.co"}, envMap(map[string]string{"FLEET_SERVER_TOKEN": "t"}), noFile)
		if cfg2.ServerURL != "http://127.0.0.1:8080" {
			t.Errorf("server = %q, want default :8080", cfg2.ServerURL)
		}
	})

	t.Run("missing email errors", func(t *testing.T) {
		_, err := Resolve(Flags{}, envMap(map[string]string{"FLEET_SERVER_TOKEN": "t"}), noFile)
		if err == nil || !strings.Contains(err.Error(), "email") {
			t.Fatalf("want email error, got %v", err)
		}
	})

	t.Run("missing token errors, and the error never leaks a token value", func(t *testing.T) {
		_, err := Resolve(Flags{Email: "a@b.co"}, envMap(map[string]string{}), noFile)
		if err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("want token error, got %v", err)
		}
	})

	t.Run("token-file is read and trimmed; CHAT_SERVER_TOKEN fallback", func(t *testing.T) {
		rf := func(p string) ([]byte, error) {
			if p == "/secret" {
				return []byte("  filetoken\n"), nil
			}
			return nil, errors.New("nope")
		}
		cfg, err := Resolve(Flags{Email: "a@b.co", TokenFile: "/secret"}, envMap(nil), rf)
		if err != nil || cfg.Token != "filetoken" {
			t.Fatalf("token-file: token=%q err=%v", cfg.Token, err)
		}
		// env fallback to CHAT_SERVER_TOKEN when no file.
		cfg2, _ := Resolve(Flags{Email: "a@b.co"}, envMap(map[string]string{"CHAT_SERVER_TOKEN": "chattok"}), noFile)
		if cfg2.Token != "chattok" {
			t.Errorf("CHAT_SERVER_TOKEN fallback = %q", cfg2.Token)
		}
	})
}
