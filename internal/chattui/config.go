// Package chattui is the `fleet chat` terminal UI (#457): a Bubble Tea/Lipgloss
// front-end for chatting with the fleet agent from the command line. It is a
// thin SSE client of the running server's POST /chat — it never builds the agent
// in-process, so every turn still flows through the one governed run loop
// (agentcore.Run), the rootless-Podman tool sandbox, and host-side credential
// brokering. The TUI only renders; the server owns execution, persistence, and
// governance.
package chattui

import (
	"fmt"
	"os"
	"strings"
)

// Config is the resolved connection identity for `fleet chat`. The Token is the
// shared server secret a host-local CLI legitimately holds; it is NEVER logged,
// printed, or accepted on argv (only via env or a 0600 token file), so a verbose
// run can't leak it.
type Config struct {
	ServerURL string // e.g. http://127.0.0.1:8080
	Email     string // X-User-Email — the authenticated identity (audit-attributed)
	Token     string // X-Chat-Server-Token shared secret
	Model     string // optional per-turn model slug ("" = server/conversation default)
	Persona   string // optional persona for a new conversation
}

// Flags are the command-line overrides; empty fields fall back to env.
type Flags struct {
	Server    string
	Email     string
	TokenFile string
	Model     string
	Persona   string
}

// getenv is the environment accessor (injectable for tests).
type getenv func(string) string

// ReadFile is the token-file reader (injectable for tests).
type readFile func(string) ([]byte, error)

// Resolve builds a Config from flags → env, reading the token from a file when
// --token-file is given. Resolution order (flag wins):
//
//	server: --server | $FLEET_CHAT_URL | http://$FLEET_SERVER_ADDR | http://127.0.0.1:8080
//	email:  --email  | $FLEET_USER_EMAIL                              (required)
//	token:  --token-file (file contents) | $FLEET_SERVER_TOKEN | $CHAT_SERVER_TOKEN (required)
//
// It returns a clear, actionable error when a required value is missing, and
// NEVER includes the token value in any error.
func Resolve(f Flags, env getenv, rf readFile) (Config, error) {
	cfg := Config{
		Model:   strings.TrimSpace(f.Model),
		Persona: strings.TrimSpace(f.Persona),
	}

	// Server URL.
	switch {
	case strings.TrimSpace(f.Server) != "":
		cfg.ServerURL = strings.TrimSpace(f.Server)
	case strings.TrimSpace(env("FLEET_CHAT_URL")) != "":
		cfg.ServerURL = strings.TrimSpace(env("FLEET_CHAT_URL"))
	case strings.TrimSpace(env("FLEET_SERVER_ADDR")) != "":
		cfg.ServerURL = "http://" + strings.TrimSpace(env("FLEET_SERVER_ADDR"))
	default:
		cfg.ServerURL = "http://127.0.0.1:8080"
	}
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")

	// Email (required) — never inferred from $USER; the audit trail needs a real
	// identity the operator chose.
	cfg.Email = strings.ToLower(strings.TrimSpace(firstNonEmpty(f.Email, env("FLEET_USER_EMAIL"))))
	if cfg.Email == "" {
		return Config{}, fmt.Errorf("no user email: pass --email <you@example.com> or set FLEET_USER_EMAIL")
	}

	// Token (required) — file first (so it can live 0600 outside the env), then env.
	if tf := strings.TrimSpace(f.TokenFile); tf != "" {
		b, err := rf(tf)
		if err != nil {
			return Config{}, fmt.Errorf("read --token-file %q: %w", tf, err)
		}
		cfg.Token = strings.TrimSpace(string(b))
	}
	if cfg.Token == "" {
		cfg.Token = strings.TrimSpace(firstNonEmpty(env("FLEET_SERVER_TOKEN"), env("CHAT_SERVER_TOKEN")))
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("no server token: set FLEET_SERVER_TOKEN (or CHAT_SERVER_TOKEN), or pass --token-file <path> (mode 0600)")
	}
	return cfg, nil
}

// osEnv / osReadFile are the production accessors.
func osEnv(k string) string               { return os.Getenv(k) }
func osReadFile(p string) ([]byte, error) { return os.ReadFile(p) } //nolint:gosec // G304: p is operator-supplied --token-file (a deliberate local path), not request/LLM input.

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
