package chattui

import (
	"errors"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	noFile := func(string) ([]byte, error) { return nil, errors.New("no file") }
	noEnvFile := func(string, ...string) (map[string]string, error) { return nil, nil }
	envMap := func(m map[string]string) getenv {
		return func(k string) string { return m[k] }
	}
	// envFileFrom builds a fake env-file reader from path → (key → value). A path
	// absent from the map behaves like a missing file (empty, no error), matching
	// creds.ReadEnvValues.
	envFileFrom := func(files map[string]map[string]string) envValuesReader {
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

	t.Run("flags win; server normalized", func(t *testing.T) {
		cfg, err := Resolve(
			Flags{Server: "http://host:9000/", Email: "Me@Example.com", Model: "x/y"},
			envMap(map[string]string{"FLEET_SERVER_TOKEN": "tok"}), noFile, noEnvFile)
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
			envMap(map[string]string{"FLEET_SERVER_TOKEN": "t", "FLEET_SERVER_ADDR": "127.0.0.1:9999"}), noFile, noEnvFile)
		if cfg.ServerURL != "http://127.0.0.1:9999" {
			t.Errorf("server = %q, want from FLEET_SERVER_ADDR", cfg.ServerURL)
		}
		cfg2, _ := Resolve(Flags{Email: "a@b.co"}, envMap(map[string]string{"FLEET_SERVER_TOKEN": "t"}), noFile, noEnvFile)
		if cfg2.ServerURL != "http://127.0.0.1:8080" {
			t.Errorf("server = %q, want default :8080", cfg2.ServerURL)
		}
	})

	t.Run("missing email errors", func(t *testing.T) {
		_, err := Resolve(Flags{}, envMap(map[string]string{"FLEET_SERVER_TOKEN": "t"}), noFile, noEnvFile)
		if err == nil || !strings.Contains(err.Error(), "email") {
			t.Fatalf("want email error, got %v", err)
		}
	})

	t.Run("missing token errors, and the error never leaks a token value", func(t *testing.T) {
		_, err := Resolve(Flags{Email: "a@b.co"}, envMap(map[string]string{}), noFile, noEnvFile)
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
		cfg, err := Resolve(Flags{Email: "a@b.co", TokenFile: "/secret"}, envMap(nil), rf, noEnvFile)
		if err != nil || cfg.Token != "filetoken" {
			t.Fatalf("token-file: token=%q err=%v", cfg.Token, err)
		}
		// env fallback to CHAT_SERVER_TOKEN when no file.
		cfg2, _ := Resolve(Flags{Email: "a@b.co"}, envMap(map[string]string{"CHAT_SERVER_TOKEN": "chattok"}), noFile, noEnvFile)
		if cfg2.Token != "chattok" {
			t.Errorf("CHAT_SERVER_TOKEN fallback = %q", cfg2.Token)
		}
	})

	t.Run("token auto-discovered from the server env file (the #480 path)", func(t *testing.T) {
		// No --token-file, no token in the process env: the token comes from the
		// default .env.local that an on-box operator can read.
		evf := envFileFrom(map[string]map[string]string{
			".env.local": {"FLEET_SERVER_TOKEN": "envfiletok", "FLEET_SERVER_ADDR": "127.0.0.1:7000"},
		})
		cfg, err := Resolve(Flags{Email: "a@b.co"}, envMap(nil), noFile, evf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Token != "envfiletok" {
			t.Errorf("token = %q, want discovered from env file", cfg.Token)
		}
		if cfg.ServerURL != "http://127.0.0.1:7000" {
			t.Errorf("server = %q, want discovered FLEET_SERVER_ADDR", cfg.ServerURL)
		}
	})

	t.Run("explicit token/server win over the env file", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			".env.local": {"FLEET_SERVER_TOKEN": "envfiletok", "FLEET_SERVER_ADDR": "127.0.0.1:7000"},
		})
		cfg, _ := Resolve(
			Flags{Email: "a@b.co", Server: "http://explicit:1234"},
			envMap(map[string]string{"FLEET_SERVER_TOKEN": "envtok"}), noFile, evf)
		if cfg.Token != "envtok" {
			t.Errorf("token = %q, want process-env to win over env file", cfg.Token)
		}
		if cfg.ServerURL != "http://explicit:1234" {
			t.Errorf("server = %q, want --server to win over env file", cfg.ServerURL)
		}
	})

	t.Run("CHAT_SERVER_TOKEN in the env file is honored", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			".env.local": {"CHAT_SERVER_TOKEN": "legacytok"},
		})
		cfg, err := Resolve(Flags{Email: "a@b.co"}, envMap(nil), noFile, evf)
		if err != nil || cfg.Token != "legacytok" {
			t.Fatalf("legacy env-file token: token=%q err=%v", cfg.Token, err)
		}
	})

	t.Run("--env-file pins a single path (no silent fallback to defaults)", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			"/custom/fleet.env": {"FLEET_SERVER_TOKEN": "customtok"},
			".env.local":        {"FLEET_SERVER_TOKEN": "WRONG"},
		})
		cfg, err := Resolve(Flags{Email: "a@b.co", EnvFile: "/custom/fleet.env"}, envMap(nil), noFile, evf)
		if err != nil || cfg.Token != "customtok" {
			t.Fatalf("--env-file: token=%q err=%v", cfg.Token, err)
		}
		// A wrong explicit path is a clear miss, not a fallback to .env.local.
		_, err = Resolve(Flags{Email: "a@b.co", EnvFile: "/does/not/exist"}, envMap(nil), noFile, evf)
		if err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("wrong --env-file should miss with a token error, got %v", err)
		}
	})

	t.Run("$FLEET_ENV_FILE pins the path when no flag is given", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			"/etc/fleet/fleet.env": {"FLEET_SERVER_TOKEN": "systok"},
		})
		cfg, err := Resolve(Flags{Email: "a@b.co"},
			envMap(map[string]string{"FLEET_ENV_FILE": "/etc/fleet/fleet.env"}), noFile, evf)
		if err != nil || cfg.Token != "systok" {
			t.Fatalf("$FLEET_ENV_FILE: token=%q err=%v", cfg.Token, err)
		}
	})

	t.Run("default candidates probed in order: .env.local then /etc/fleet/fleet.env", func(t *testing.T) {
		evf := envFileFrom(map[string]map[string]string{
			"/etc/fleet/fleet.env": {"FLEET_SERVER_TOKEN": "systok"},
		})
		// .env.local is absent here, so discovery falls through to the systemd path.
		cfg, err := Resolve(Flags{Email: "a@b.co"}, envMap(nil), noFile, evf)
		if err != nil || cfg.Token != "systok" {
			t.Fatalf("fallthrough to /etc/fleet/fleet.env: token=%q err=%v", cfg.Token, err)
		}
	})

	t.Run("unreadable env file is skipped; token error mentions the paths but not a value", func(t *testing.T) {
		deny := func(string, ...string) (map[string]string, error) { return nil, errors.New("permission denied") }
		_, err := Resolve(Flags{Email: "a@b.co"}, envMap(nil), noFile, deny)
		if err == nil || !strings.Contains(err.Error(), "token") {
			t.Fatalf("want token error on unreadable env file, got %v", err)
		}
		if !strings.Contains(err.Error(), ".env.local") {
			t.Errorf("token error should name the probed paths, got %q", err.Error())
		}
	})
}
