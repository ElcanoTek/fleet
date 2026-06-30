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

	"github.com/ElcanoTek/fleet/internal/creds"
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
	EnvFile   string // server env file to auto-discover the token/addr from
	Model     string
	Persona   string
}

// getenv is the environment accessor (injectable for tests).
type getenv func(string) string

// ReadFile is the token-file reader (injectable for tests).
type readFile func(string) ([]byte, error)

// envValuesReader reads selected KEY=VALUE pairs from a server env file without
// mutating the process environment (injectable for tests; backed by
// creds.ReadEnvValues in production).
type envValuesReader func(path string, keys ...string) (map[string]string, error)

// Env-file keys fleet chat will auto-discover, and the canonical files it probes.
const (
	envKeyServerToken       = "FLEET_SERVER_TOKEN" // canonical shared-secret key
	envKeyServerTokenLegacy = "CHAT_SERVER_TOKEN"  // legacy alias
	envKeyServerAddr        = "FLEET_SERVER_ADDR"  // chat server listen addr
)

// defaultEnvFilePaths are the two locations bootstrap writes the credential file
// to: the dev/local default, then the systemd-deployment default. Probed in
// order when neither --env-file nor $FLEET_ENV_FILE pins one explicitly.
var defaultEnvFilePaths = []string{".env.local", "/etc/fleet/fleet.env"}

// Resolve builds a Config from flags → env → the server's own env file.
// Resolution order (first non-empty wins):
//
//	server: --server | $FLEET_CHAT_URL | http://$FLEET_SERVER_ADDR | env-file FLEET_SERVER_ADDR | http://127.0.0.1:8080
//	email:  --email  | $FLEET_USER_EMAIL                                                          (required)
//	token:  --token-file (contents) | $FLEET_SERVER_TOKEN | $CHAT_SERVER_TOKEN | env-file FLEET_SERVER_TOKEN/CHAT_SERVER_TOKEN (required)
//
// The env-file step (#480) is the on-box convenience: when the token (or server
// address) wasn't given explicitly, Resolve reads it from the SAME 0600
// credential file the server reads — so an operator who can administer the box
// (i.e. read that file) can just run `fleet chat --email me@org` with no token
// wrangling. This keeps the host-side-secret invariant intact: the token is
// still never accepted on argv and never logged; the file's own permissions are
// the access gate, and reading a local file the operator already controls grants
// no access they didn't already have.
//
// It returns a clear, actionable error when a required value is missing, and
// NEVER includes the token value in any error.
func Resolve(f Flags, env getenv, rf readFile, evf envValuesReader) (Config, error) {
	cfg := Config{
		Model:   strings.TrimSpace(f.Model),
		Persona: strings.TrimSpace(f.Persona),
	}

	// Token from explicit sources first: a --token-file (so it can live 0600
	// outside the env) then the process env. NEVER read off argv.
	if tf := strings.TrimSpace(f.TokenFile); tf != "" {
		b, err := rf(tf)
		if err != nil {
			return Config{}, fmt.Errorf("read --token-file %q: %w", tf, err)
		}
		cfg.Token = strings.TrimSpace(string(b))
	}
	if cfg.Token == "" {
		cfg.Token = strings.TrimSpace(firstNonEmpty(env(envKeyServerToken), env(envKeyServerTokenLegacy)))
	}

	// Server URL from flags/env (default applied after the env-file step below).
	switch {
	case strings.TrimSpace(f.Server) != "":
		cfg.ServerURL = strings.TrimSpace(f.Server)
	case strings.TrimSpace(env("FLEET_CHAT_URL")) != "":
		cfg.ServerURL = strings.TrimSpace(env("FLEET_CHAT_URL"))
	case strings.TrimSpace(env(envKeyServerAddr)) != "":
		cfg.ServerURL = "http://" + strings.TrimSpace(env(envKeyServerAddr))
	}

	// On-box auto-discovery: fill any still-missing token/addr from the server
	// env file. Skipped entirely once both are already resolved.
	candidates := envFileCandidates(f.EnvFile, env)
	if cfg.Token == "" || cfg.ServerURL == "" {
		if vals, _ := discoverEnvValues(evf, candidates, envKeyServerToken, envKeyServerTokenLegacy, envKeyServerAddr); vals != nil {
			if cfg.Token == "" {
				cfg.Token = strings.TrimSpace(firstNonEmpty(vals[envKeyServerToken], vals[envKeyServerTokenLegacy]))
			}
			if cfg.ServerURL == "" {
				if addr := strings.TrimSpace(vals[envKeyServerAddr]); addr != "" {
					cfg.ServerURL = "http://" + addr
				}
			}
		}
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://127.0.0.1:8080"
	}
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")

	// Email (required) — never inferred. Not from $USER, and deliberately NOT
	// from the shared env file either: one box can serve several operators, so
	// the audit trail needs the identity the caller chose, not a shared default.
	cfg.Email = strings.ToLower(strings.TrimSpace(firstNonEmpty(f.Email, env("FLEET_USER_EMAIL"))))
	if cfg.Email == "" {
		return Config{}, fmt.Errorf("no user email: pass --email <you@example.com> or set FLEET_USER_EMAIL (your audit identity, so it is never guessed)")
	}

	if cfg.Token == "" {
		return Config{}, fmt.Errorf("no server token: on the box, `fleet chat` reads %s from the server env file (tried %s) — ensure you can read it, or set %s / %s, or pass --token-file <path> (mode 0600)",
			envKeyServerToken, strings.Join(candidates, ", "), envKeyServerToken, envKeyServerTokenLegacy)
	}
	return cfg, nil
}

// envFileCandidates returns the env files `fleet chat` will probe, in order. An
// explicit --env-file (or $FLEET_ENV_FILE) pins a SINGLE path, so a wrong
// explicit path is a clear miss rather than a silent fallback; otherwise the two
// locations bootstrap writes to are tried in turn.
func envFileCandidates(explicit string, env getenv) []string {
	if p := strings.TrimSpace(explicit); p != "" {
		return []string{p}
	}
	if p := strings.TrimSpace(env("FLEET_ENV_FILE")); p != "" {
		return []string{p}
	}
	return append([]string(nil), defaultEnvFilePaths...)
}

// discoverEnvValues reads the requested keys from the first readable candidate
// that yields any of them, returning the values and the path used. An unreadable
// candidate (missing file, or a 0600 file the caller can't read) is skipped, not
// fatal — so a non-admin who can't read the credential file simply gets the
// normal "no server token" error rather than a crash.
func discoverEnvValues(evf envValuesReader, candidates []string, keys ...string) (map[string]string, string) {
	if evf == nil {
		return nil, ""
	}
	for _, p := range candidates {
		vals, err := evf(p, keys...)
		if err != nil || len(vals) == 0 {
			continue
		}
		return vals, p
	}
	return nil, ""
}

// osEnv / osReadFile / osReadEnvValues are the production accessors.
func osEnv(k string) string               { return os.Getenv(k) }
func osReadFile(p string) ([]byte, error) { return os.ReadFile(p) } //nolint:gosec // G304: p is operator-supplied --token-file (a deliberate local path), not request/LLM input.

func osReadEnvValues(path string, keys ...string) (map[string]string, error) {
	return creds.ReadEnvValues(path, keys...)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
