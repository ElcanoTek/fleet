package chattui

import (
	"errors"
	"strings"
	"testing"
)

// Shared test doubles for Resolve's injected accessors.
func noFile(string) ([]byte, error)                          { return nil, errors.New("no file") }
func noEnvFile(string, ...string) (map[string]string, error) { return nil, nil }
func envMap(m map[string]string) getenv                      { return func(k string) string { return m[k] } }

// envFileFrom builds a fake env-file reader from path → (key → value). A path
// absent from the map behaves like a missing file (empty, no error), matching
// creds.ReadEnvValues.
func envFileFrom(files map[string]map[string]string) envValuesReader {
	return func(path string, keys ...string) (map[string]string, error) {
		f, ok := files[path]
		if !ok {
			return nil, nil
		}
		out := map[string]string{}
		for _, k := range keys {
			if v, ok := f[k]; ok {
				out[k] = v
			}
		}
		return out, nil
	}
}

func TestResolve(t *testing.T) {
	t.Run("flags win; server normalized", func(t *testing.T) {
		cfg, err := Resolve(
			Flags{Server: "http://host:9000/", Email: "Me@Example.com", Model: "  x/y  ", Persona: "  my-persona  "},
			envMap(map[string]string{"FLEET_API_KEY": "key"}), noFile, noEnvFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ServerURL != "http://host:9000" {
			t.Errorf("server = %q, want trimmed http://host:9000", cfg.ServerURL)
		}
		if cfg.Email != "me@example.com" {
			t.Errorf("email = %q, want lowercased", cfg.Email)
		}
		if cfg.APIKey != "key" || cfg.Model != "x/y" {
			t.Errorf("key/model = %q/%q", cfg.APIKey, cfg.Model)
		}
		if cfg.Persona != "my-persona" {
			t.Errorf("persona = %q, want %q", cfg.Persona, "my-persona")
		}
	})

	t.Run("server default + FLEET_SERVER_ADDR", func(t *testing.T) {
		cfg, _ := Resolve(Flags{},
			envMap(map[string]string{"FLEET_API_KEY": "k", "FLEET_SERVER_ADDR": "127.0.0.1:9999"}), noFile, noEnvFile)
		if cfg.ServerURL != "http://127.0.0.1:9999" {
			t.Errorf("server = %q, want from FLEET_SERVER_ADDR", cfg.ServerURL)
		}
		cfg2, _ := Resolve(Flags{}, envMap(map[string]string{"FLEET_API_KEY": "k"}), noFile, noEnvFile)
		if cfg2.ServerURL != "http://127.0.0.1:8080" {
			t.Errorf("server = %q, want default :8080", cfg2.ServerURL)
		}
	})

	t.Run("the email is optional: the key is the identity", func(t *testing.T) {
		cfg, err := Resolve(Flags{}, envMap(map[string]string{"FLEET_API_KEY": "k"}), noFile, noEnvFile)
		if err != nil || cfg.Email != "" {
			t.Fatalf("email = %q err=%v", cfg.Email, err)
		}
	})

	t.Run("missing key errors with the command that mints one, and never leaks a value", func(t *testing.T) {
		_, err := Resolve(Flags{Email: "a@b.co"}, envMap(map[string]string{"HOME": "/home/a"}), noFile, noEnvFile)
		if err == nil || !strings.Contains(err.Error(), "no API key") || !strings.Contains(err.Error(), "--type user") ||
			!strings.Contains(err.Error(), "/home/a/.config/fleet/api-key") {
			t.Fatalf("want key error, got %v", err)
		}
	})

	t.Run("key-file is read and trimmed, then $FLEET_API_KEY, then ~/.config/fleet/api-key", func(t *testing.T) {
		rf := func(p string) ([]byte, error) {
			switch p {
			case "/secret":
				return []byte("  filekey\n"), nil
			case "/home/a/.config/fleet/api-key":
				return []byte("homekey\n"), nil
			}
			return nil, errors.New("nope")
		}
		cfg, err := Resolve(Flags{KeyFile: "/secret"}, envMap(map[string]string{"FLEET_API_KEY": "envkey", "HOME": "/home/a"}), rf, noEnvFile)
		if err != nil || cfg.APIKey != "filekey" {
			t.Fatalf("key-file: key=%q err=%v", cfg.APIKey, err)
		}
		cfg, err = Resolve(Flags{}, envMap(map[string]string{"FLEET_API_KEY": "envkey", "HOME": "/home/a"}), rf, noEnvFile)
		if err != nil || cfg.APIKey != "envkey" {
			t.Fatalf("env: key=%q err=%v", cfg.APIKey, err)
		}
		cfg, err = Resolve(Flags{}, envMap(map[string]string{"HOME": "/home/a"}), rf, noEnvFile)
		if err != nil || cfg.APIKey != "homekey" {
			t.Fatalf("default file: key=%q err=%v", cfg.APIKey, err)
		}
	})
}

// The shared server token is retired (ADR-0108): a caller still configured
// with it gets an error naming the replacement, never a silent fallback, and
// the env file's token is never read as a credential.
func TestResolveRefusesTheRetiredServerToken(t *testing.T) {
	t.Run("--token-file", func(t *testing.T) {
		_, err := Resolve(Flags{TokenFile: "/secret"}, envMap(map[string]string{"FLEET_API_KEY": "k"}), noFile, noEnvFile)
		if err == nil || !strings.Contains(err.Error(), "ADR-0108") || !strings.Contains(err.Error(), "--type user") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no key: the error says the token is retired", func(t *testing.T) {
		_, err := Resolve(Flags{}, envMap(map[string]string{"FLEET_SERVER_TOKEN": "stale-token"}), noFile, noEnvFile)
		if err == nil || !strings.Contains(err.Error(), "ADR-0108") || strings.Contains(err.Error(), "stale-token") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the env file's token is not a credential", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			".env.local": {"FLEET_SERVER_TOKEN": "envfiletok", "CHAT_SERVER_TOKEN": "legacytok"},
		})
		if cfg, err := Resolve(Flags{}, envMap(nil), noFile, evf); err == nil {
			t.Fatalf("a server token was read as the key: %q", cfg.APIKey)
		}
	})
}

// TestResolveEnvFileDiscovery covers the #480 on-box auto-discovery path: the
// server address comes from the server's own env file when not given
// explicitly. (The token it once also read is retired, ADR-0108.)
func TestResolveEnvFileDiscovery(t *testing.T) {
	key := envMap(map[string]string{"FLEET_API_KEY": "k"})
	t.Run("address auto-discovered from the server env file", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			".env.local": {"FLEET_SERVER_ADDR": "127.0.0.1:7000"},
		})
		cfg, err := Resolve(Flags{}, key, noFile, evf)
		if err != nil || cfg.ServerURL != "http://127.0.0.1:7000" {
			t.Fatalf("server = %q err=%v, want discovered FLEET_SERVER_ADDR", cfg.ServerURL, err)
		}
	})

	t.Run("an explicit server wins over the env file", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			".env.local": {"FLEET_SERVER_ADDR": "127.0.0.1:7000"},
		})
		cfg, _ := Resolve(Flags{Server: "http://explicit:1234"}, key, noFile, evf)
		if cfg.ServerURL != "http://explicit:1234" {
			t.Errorf("server = %q, want --server to win over env file", cfg.ServerURL)
		}
	})

	t.Run("--env-file pins a single path (no silent fallback to defaults)", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			"/custom/fleet.env": {"FLEET_SERVER_ADDR": "127.0.0.1:7001"},
			".env.local":        {"FLEET_SERVER_ADDR": "127.0.0.1:9"},
		})
		cfg, err := Resolve(Flags{EnvFile: "/custom/fleet.env"}, key, noFile, evf)
		if err != nil || cfg.ServerURL != "http://127.0.0.1:7001" {
			t.Fatalf("--env-file: server=%q err=%v", cfg.ServerURL, err)
		}
		cfg, _ = Resolve(Flags{EnvFile: "/does/not/exist"}, key, noFile, evf)
		if cfg.ServerURL != "http://127.0.0.1:8080" {
			t.Fatalf("a wrong --env-file fell back to %q", cfg.ServerURL)
		}
	})

	t.Run("default candidates probed in order: .env.local then /etc/fleet/fleet.env", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			"/etc/fleet/fleet.env": {"FLEET_SERVER_ADDR": "127.0.0.1:7002"},
		})
		cfg, err := Resolve(Flags{}, key, noFile, evf)
		if err != nil || cfg.ServerURL != "http://127.0.0.1:7002" {
			t.Fatalf("fallthrough to /etc/fleet/fleet.env: server=%q err=%v", cfg.ServerURL, err)
		}
	})

	t.Run("an unreadable env file is skipped", func(t *testing.T) {
		deny := func(string, ...string) (map[string]string, error) { return nil, errors.New("permission denied") }
		cfg, err := Resolve(Flags{}, key, noFile, deny)
		if err != nil || cfg.ServerURL != "http://127.0.0.1:8080" {
			t.Fatalf("server=%q err=%v", cfg.ServerURL, err)
		}
	})
}
