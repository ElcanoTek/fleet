package admincli

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// cmdConfig dispatches `fleet config set-openrouter-key|set-auth-pubkey` — the
// operator-friendly writers for the two credentials every deployment needs
// beyond what bootstrap generates itself: the OpenRouter API key (server env)
// and the Elcano SSO verification key (web env). Both exist so nobody has to
// hand-edit a 0600 env file; bootstrap's interactive prompts call the same
// underlying writes.
func cmdConfig(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet config set-openrouter-key | set-auth-pubkey")
	}
	switch argv[0] {
	case "set-openrouter-key":
		return configSetOpenRouterKey(argv[1:])
	case "set-auth-pubkey":
		return configSetAuthPubkey(argv[1:])
	default:
		return errf(1, "unknown config subcommand %q", argv[0])
	}
}

// serverEnvFile resolves the BACKEND env file for config writes: --env-file,
// else FLEET_ENV_FILE, else the systemd deployment's /etc/fleet/fleet.env when
// it exists, else .env.local. The /etc probe is what makes a bare
// `fleet config set-openrouter-key` on a provisioned box hit the file the unit
// actually reads (envFilePath alone would target a stray ./.env.local there).
func serverEnvFile(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_ENV_FILE")); v != "" {
		return v
	}
	// Probe the DIRECTORY, not the file: on a systemd-provisioned box the
	// canonical path is right even when the file hasn't been created yet
	// (SetEnvKey creates it 0600) — falling back to a CWD-relative file there
	// would scatter credentials outside the deployment.
	if fi, err := os.Stat("/etc/fleet"); err == nil && fi.IsDir() {
		return "/etc/fleet/fleet.env"
	}
	return ".env.local"
}

// webEnvFile resolves the WEB-TIER env file (where AUTH_* SSO keys live):
// --env-file, else the systemd deployment's /etc/fleet/fleet-web.env when it
// exists, else web/.env.local (the Next.js dev convention).
func webEnvFile(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	// Directory probe, same reasoning as serverEnvFile.
	if fi, err := os.Stat("/etc/fleet"); err == nil && fi.IsDir() {
		return "/etc/fleet/fleet-web.env"
	}
	return "web/.env.local"
}

// promptLine reads one visible line from stdin with a stderr prompt — for
// non-secret pastes (a public key) where echo helps the operator see the paste
// landed intact.
func promptLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// configSetOpenRouterKey upserts OPENROUTER_API_KEY into the server env file.
// The key is prompted hidden on a TTY (or --key - reads stdin) so it never
// lands on argv or in shell history.
func configSetOpenRouterKey(argv []string) int {
	fs := flag.NewFlagSet("config set-openrouter-key", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "server env file (default FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	key := fs.String("key", "", `API key ("-" reads from stdin; omit to prompt hidden)`)
	_, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	val, err := resolveSecret(*key, "OpenRouter API key: ", false)
	if err != nil {
		return errf(1, "%v", err)
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return errf(1, "empty key")
	}
	// Soft shape check only: OpenRouter keys are sk-or-…, but the prefix is
	// theirs to change, so warn rather than fail closed.
	if !strings.HasPrefix(val, "sk-or-") {
		fmt.Fprintln(os.Stderr, "note: key does not start with sk-or- — double-check it is an OpenRouter key")
	}
	path := serverEnvFile(*envFile)
	if err := creds.SetEnvKey(path, "OPENROUTER_API_KEY", val); err != nil {
		return errf(5, "write %s: %v", path, err)
	}
	fmt.Printf("set OPENROUTER_API_KEY in %s\n", path)
	fmt.Println("apply with: fleet restart   (systemctl restart fleet)")
	return 0
}

// ParseAuthPubkey normalizes and validates an Elcano AUTH_SIGNING_PUBKEY value:
// it accepts either the bare standard-base64 key or a pasted
// `AUTH_SIGNING_PUBKEY=<base64>` line (the exact `auth pubkey` output), strips
// quotes/whitespace, and requires the decoded key to be exactly 32 bytes
// (ed25519.PublicKeySize) — the same fail-closed shape check the web verifier
// applies, so a bad paste is caught here instead of as silent login failures.
func ParseAuthPubkey(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if eq := strings.IndexByte(v, '='); eq > 0 && strings.EqualFold(strings.TrimSpace(v[:eq]), "AUTH_SIGNING_PUBKEY") {
		v = v[eq+1:]
	}
	v = strings.Trim(strings.TrimSpace(v), `"'`)
	if v == "" {
		return "", fmt.Errorf("empty key")
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", fmt.Errorf("not valid standard base64 (paste the value from `auth pubkey`): %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return "", fmt.Errorf("decoded to %d bytes, want %d (an Ed25519 public key)", len(b), ed25519.PublicKeySize)
	}
	return v, nil
}

// configSetAuthPubkey upserts AUTH_SIGNING_PUBKEY (+ optional AUTH_LOGIN_URL /
// AUTH_COOKIE_DOMAIN) into the web-tier env file, enabling Elcano-style SSO.
// The value comes from the positional arg, --from <file>, or an interactive
// paste prompt; all three accept `auth pubkey`'s KEY=value output verbatim.
// Only the PUBLIC key is ever handled here — the signing (private) key stays on
// the auth host and fleet never needs it.
func configSetAuthPubkey(argv []string) int {
	fs := flag.NewFlagSet("config set-auth-pubkey", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "web env file (default /etc/fleet/fleet-web.env when present, else web/.env.local)")
	from := fs.String("from", "", "read the key from this file (e.g. scp'd `auth pubkey` output)")
	loginURL := fs.String("login-url", "", "auth service base URL (AUTH_LOGIN_URL; optional — the web tier defaults it)")
	cookieDomain := fs.String("cookie-domain", "", "shared cookie domain, e.g. elcanotek.com (AUTH_COOKIE_DOMAIN; needed for cross-subdomain logout)")
	value, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}

	raw := value
	switch {
	case raw != "" && *from != "":
		return errf(1, "pass the key either as an argument or via --from, not both")
	case *from != "":
		b, err := os.ReadFile(*from)
		if err != nil {
			return errf(1, "read %s: %v", *from, err)
		}
		raw = string(b)
	case raw == "":
		v, err := promptLine("AUTH_SIGNING_PUBKEY (paste the `auth pubkey` line or the bare base64): ")
		if err != nil {
			return errf(1, "%v", err)
		}
		raw = v
	}

	key, err := ParseAuthPubkey(raw)
	if err != nil {
		return errf(1, "invalid AUTH_SIGNING_PUBKEY: %v", err)
	}

	path := webEnvFile(*envFile)
	if err := creds.SetEnvKey(path, "AUTH_SIGNING_PUBKEY", key); err != nil {
		return errf(5, "write %s: %v", path, err)
	}
	if v := strings.TrimSpace(*loginURL); v != "" {
		if err := creds.SetEnvKey(path, "AUTH_LOGIN_URL", v); err != nil {
			return errf(5, "write %s: %v", path, err)
		}
	}
	if v := strings.TrimSpace(*cookieDomain); v != "" {
		if err := creds.SetEnvKey(path, "AUTH_COOKIE_DOMAIN", v); err != nil {
			return errf(5, "write %s: %v", path, err)
		}
	}
	fmt.Printf("set AUTH_SIGNING_PUBKEY in %s — Elcano SSO enabled\n", path)
	fmt.Println("apply with: systemctl restart fleet-web")
	return 0
}
