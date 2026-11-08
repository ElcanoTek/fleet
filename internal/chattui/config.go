// Package chattui is the `fleet chat` terminal UI (#457): a Bubble Tea/Lipgloss
// front-end for chatting with the fleet agent from the command line. It is a
// thin SSE client of the running server's POST /chat — it never builds the agent
// in-process, so every turn still flows through the one governed run loop
// (agentcore.Run), the rootless-Podman tool sandbox, and host-side credential
// brokering. The TUI only renders; the server owns execution, persistence, and
// governance.
package chattui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// Config is the resolved connection identity for `fleet chat`. The APIKey is
// a user API key (`fleet sched apikey create <name> --type user --user
// <email>`, ADR-0108): the server acts as the account it names. It is NEVER
// logged, printed, or accepted on argv (only via env or a 0600 key file), so a
// verbose run can't leak it.
type Config struct {
	ServerURL string // e.g. http://127.0.0.1:8080
	// Email is the account the key acts as: the --email/$FLEET_USER_EMAIL
	// the caller expects (CheckIdentity refuses a key for another account),
	// else filled in from GET /me. It is never sent: the key is the identity.
	Email   string
	APIKey  string // X-API-Key: a user API key
	Model   string // optional per-turn model slug ("" = server/conversation default)
	Persona string // optional persona for a new conversation

	// ModelNewConversationsOnly sends Model only on a turn that starts a new
	// conversation, leaving an existing conversation's stored model alone
	// (`fleet acp --model` picks the model for new sessions; a later switch
	// made in the web UI must not be silently reverted by the next prompt).
	// Off for `fleet chat`, where --model is an override for every turn.
	ModelNewConversationsOnly bool
	// InputIDsUserUnique declares that the input_ids this client sends are
	// unique per user (`fleet acp`'s are random or session-scoped), so the
	// server may look a first submission's key up across conversations.
	InputIDsUserUnique bool

	// ClientName is the X-Fleet-Client attribution label ("" = "fleet-chat").
	// `fleet acp` sets "fleet-acp" so a server log can tell the two apart.
	ClientName string
	// PublicURL is the web UI's base URL (FLEET_PUBLIC_BASE_URL, else
	// FLEET_PUBLIC_URL, from the env or the server env file), used to build
	// deep links such as an approval card's conversation. "" when unset.
	PublicURL string

	// DefaultConnectors makes a NEW conversation open with the optional
	// connectors a new web chat starts with (GET /mcp-servers). `fleet chat`
	// sets it; without it the server binds none (it never adds defaults).
	DefaultConnectors bool
	// Connectors are explicit --connector names for a new conversation; they
	// replace the defaults. Lower-cased, de-duplicated.
	Connectors []string
}

// Flags are the command-line overrides; empty fields fall back to env.
type Flags struct {
	Server  string
	Email   string
	KeyFile string
	// TokenFile is the retired --token-file (the server token, ADR-0108):
	// accepted only so its use gets an error naming the replacement.
	TokenFile string
	EnvFile   string // server env file to auto-discover the server address from
	Model     string
	Persona   string
	// PublicURL is an explicit web UI base URL for deep links. It always
	// wins, and is the only source used when the server was chosen
	// explicitly (--server / $FLEET_CHAT_URL), since an ambient public URL
	// may belong to a different deployment.
	PublicURL string
	// Connectors are repeated --connector names for a new conversation.
	Connectors []string
}

// getenv is the environment accessor (injectable for tests).
type getenv func(string) string

// ReadFile is the token-file reader (injectable for tests).
type readFile func(string) ([]byte, error)

// envValuesReader reads selected KEY=VALUE pairs from a server env file without
// mutating the process environment (injectable for tests; backed by
// creds.ReadEnvValues in production).
type envValuesReader func(path string, keys ...string) (map[string]string, error)

// defaultEnvFilePaths are the two locations bootstrap writes the credential file
// to: the dev/local default, then the systemd-deployment default. Probed in
// order when neither --env-file nor $FLEET_ENV_FILE pins one explicitly.
var defaultEnvFilePaths = []string{".env.local", "/etc/fleet/fleet.env"}

// Resolve builds a Config from flags → env → the server's own env file.
// Resolution order (first non-empty wins):
//
//	server: --server | $FLEET_CHAT_URL | http://$FLEET_SERVER_ADDR | env-file FLEET_SERVER_ADDR | http://127.0.0.1:8080
//	key:    --key-file (contents) | $FLEET_API_KEY | ~/.config/fleet/api-key (contents)    (required)
//	email:  --email  | $FLEET_USER_EMAIL                         (optional: the account the key must act as)
//
// The key is a user API key (ADR-0108); the server acts as the account it
// names. The shared server token this used to read from the server env file
// is retired: --token-file and $FLEET_SERVER_TOKEN are refused with an error
// naming the replacement. The env file still supplies the server address.
//
// It returns a clear, actionable error when a required value is missing, and
// NEVER includes the key value in any error.
func Resolve(f Flags, env getenv, rf readFile, evf envValuesReader) (Config, error) {
	cfg := Config{
		Model:      strings.TrimSpace(f.Model),
		Persona:    strings.TrimSpace(f.Persona),
		Connectors: normalizeConnectors(f.Connectors),
	}

	// The retired server token: refuse it by name, before anything else, so
	// an old invocation learns what replaced it.
	if strings.TrimSpace(f.TokenFile) != "" {
		return Config{}, errors.New(serviceTokenRetired("--token-file"))
	}
	// Key from explicit sources first: a --key-file (so it can live 0600
	// outside the env) then the process env, then the per-user default file.
	// NEVER read off argv.
	if kf := strings.TrimSpace(f.KeyFile); kf != "" {
		b, err := rf(kf)
		if err != nil {
			return Config{}, fmt.Errorf("read --key-file %q: %w", kf, err)
		}
		cfg.APIKey = strings.TrimSpace(string(b))
	}
	if cfg.APIKey == "" {
		cfg.APIKey = strings.TrimSpace(env("FLEET_API_KEY"))
	}
	defaultKeyFile := defaultKeyFilePath(env)
	if cfg.APIKey == "" && defaultKeyFile != "" {
		if b, err := rf(defaultKeyFile); err == nil {
			cfg.APIKey = strings.TrimSpace(string(b))
		}
	}

	// Server URL from flags/env (default applied after the env-file step below).
	serverExplicit := strings.TrimSpace(f.Server) != "" || strings.TrimSpace(env("FLEET_CHAT_URL")) != ""
	switch {
	case strings.TrimSpace(f.Server) != "":
		cfg.ServerURL = strings.TrimSpace(f.Server)
	case strings.TrimSpace(env("FLEET_CHAT_URL")) != "":
		cfg.ServerURL = strings.TrimSpace(env("FLEET_CHAT_URL"))
	case strings.TrimSpace(env("FLEET_SERVER_ADDR")) != "":
		cfg.ServerURL = "http://" + strings.TrimSpace(env("FLEET_SERVER_ADDR"))
	}

	// On-box auto-discovery: fill a still-missing address from the server
	// env file. Skipped entirely once it is resolved.
	candidates := envFileCandidates(f.EnvFile, env)
	// serverFile is the env file that supplied the address, if any — the
	// deployment this client is talking to.
	serverFile := ""
	if cfg.ServerURL == "" {
		if vals, path := discoverEnvValues(evf, candidates, "FLEET_SERVER_ADDR"); vals != nil {
			if cfg.ServerURL == "" {
				if addr := strings.TrimSpace(vals["FLEET_SERVER_ADDR"]); addr != "" {
					cfg.ServerURL, serverFile = "http://"+addr, path
				}
			}
		}
	}
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://127.0.0.1:8080"
	}

	// Public web URL (non-secret, best-effort) must describe the SAME
	// deployment the token and address came from, or a deep link would send
	// the user to another fleet. In order:
	//
	//   - an explicit --public-url always wins;
	//   - when the server was chosen explicitly (--server / $FLEET_CHAT_URL),
	//     nothing ambient is trusted: an env or file public URL describes
	//     whatever deployment that environment belongs to;
	//   - when an env file supplied the token or address, only THAT file's
	//     public URL is used — the process env may belong to another one;
	//   - otherwise the env, then an explicitly pinned env file.
	//
	// It is never probed across the default candidates on its own.
	cfg.PublicURL = strings.TrimSpace(f.PublicURL)
	pinned := strings.TrimSpace(f.EnvFile) != "" || strings.TrimSpace(env("FLEET_ENV_FILE")) != ""
	publicFile := ""
	switch {
	case cfg.PublicURL != "" || serverExplicit:
	case serverFile != "":
		publicFile = serverFile
	default:
		cfg.PublicURL = strings.TrimSpace(firstNonEmpty(env("FLEET_PUBLIC_BASE_URL"), env("FLEET_PUBLIC_URL")))
		if pinned {
			publicFile = candidates[0]
		}
	}
	if cfg.PublicURL == "" && publicFile != "" && evf != nil {
		if vals, err := evf(publicFile, "FLEET_PUBLIC_BASE_URL", "FLEET_PUBLIC_URL"); err == nil {
			cfg.PublicURL = strings.TrimSpace(firstNonEmpty(vals["FLEET_PUBLIC_BASE_URL"], vals["FLEET_PUBLIC_URL"]))
		}
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")

	// Email (optional) — the account the caller expects the key to act as.
	// Never inferred from $USER or the shared env file: the key is the
	// identity, and this is only a check on it (CheckIdentity).
	cfg.Email = strings.ToLower(strings.TrimSpace(firstNonEmpty(f.Email, env("FLEET_USER_EMAIL"))))

	if cfg.APIKey == "" {
		// The shared server token this used to read from the env file is
		// retired (ADR-0108); the error says so, since an old setup still
		// holding one would otherwise wonder why it stopped working.
		where := "$FLEET_API_KEY or --key-file <path> (mode 0600)"
		if defaultKeyFile != "" {
			where += ", or save it to " + defaultKeyFile
		}
		return Config{}, fmt.Errorf("no API key (the shared server token is retired, ADR-0108): create a user key on the box with `fleet sched apikey create <name> --type user --user <you@example.com>` and pass it as %s", where)
	}
	return cfg, nil
}

// serviceTokenRetired is the error for a caller still configured with the
// retired server token (ADR-0108).
func serviceTokenRetired(source string) string {
	return source + " held the shared server token, which was removed (ADR-0108): create a user API key with `fleet sched apikey create <name> --type user --user <you@example.com>` and pass it as $FLEET_API_KEY or --key-file <path>"
}

// defaultKeyFilePath is the per-user key file Resolve falls back to,
// ~/.config/fleet/api-key ("" when $HOME is unset).
func defaultKeyFilePath(env getenv) string {
	if h := strings.TrimSpace(env("HOME")); h != "" {
		return filepath.Join(h, ".config", "fleet", "api-key")
	}
	return ""
}

// ResolveFromEnvironment is Resolve against the real process environment,
// filesystem and server env file — the entry point other CLI front-ends
// (`fleet acp`) use so they resolve identity exactly as `fleet chat` does.
func ResolveFromEnvironment(f Flags) (Config, error) {
	return Resolve(f, osEnv, osReadFile, osReadEnvValues)
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
func osEnv(k string) string               { return envreg.Get(k) }
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
