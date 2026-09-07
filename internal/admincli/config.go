package admincli

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// cmdConfig dispatches the operator-friendly credential writers — the keys a
// deployment needs beyond what bootstrap generates itself: the OpenRouter API
// key (server env), the Elcano SSO verification key (web env), and the
// Browserbase API key that mints hosted-browser live views (server env, #987).
// They exist so nobody has to hand-edit a 0600 env file; bootstrap's
// interactive prompts call the same underlying writes.
func cmdConfig(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet config set-openrouter-key | set-auth-pubkey | set-browserbase-key | set-env <KEY> | unset-env <KEY>")
	}
	switch argv[0] {
	case "set-env":
		return configSetEnv(argv[1:])
	case "unset-env":
		return configUnsetEnv(argv[1:])
	case "set-openrouter-key":
		return configSetOpenRouterKey(argv[1:])
	case "set-auth-pubkey":
		return configSetAuthPubkey(argv[1:])
	case "set-browserbase-key":
		return configSetBrowserbaseKey(argv[1:])
	default:
		return errf(1, "unknown config subcommand %q", argv[0])
	}
}

// serverEnvFile resolves the BACKEND env file for config reads and writes:
// --env-file, else FLEET_ENV_FILE, else the systemd deployment's
// /etc/fleet/fleet.env when /etc/fleet exists, else .env.local. It is a thin
// wrapper over config.ResolveEnvFile — the ONE resolver the in-binary
// preflight verbs (validate-config, eval, mcp test) share — so a credential
// `fleet config set-openrouter-key` writes on a provisioned box is the file
// `fleet validate-config` then reads. (A CWD-relative .env.local there would
// scatter credentials outside the deployment; see the resolver's doc.)
func serverEnvFile(flagVal string) string {
	return config.ResolveEnvFile(flagVal)
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
	printEnvApplyHint(false)
	return 0
}

// configSetBrowserbaseKey upserts BROWSERBASE_API_KEY into the server env file.
// This is the key the host-side browserbase_live_view tool uses to turn a hosted
// session id into a live-view URL for a human (#987, docs/BROWSERBASE.md) — it is
// deliberately SEPARATE from the per-user Browserbase MCP connector credential,
// which the user pastes in Settings → Connections and which is sealed in the
// database rather than the env file.
//
// Same hidden-prompt handling as the OpenRouter writer, so the key never lands on
// argv or in shell history. A restart is required because the tool is registered
// only when the key is present at startup.
func configSetBrowserbaseKey(argv []string) int {
	fs := flag.NewFlagSet("config set-browserbase-key", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "server env file (default FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	key := fs.String("key", "", `API key ("-" reads from stdin; omit to prompt hidden)`)
	_, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	val, err := resolveSecret(*key, "Browserbase API key: ", false)
	if err != nil {
		return errf(1, "%v", err)
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return errf(1, "empty key")
	}
	path := serverEnvFile(*envFile)
	if err := creds.SetEnvKey(path, "BROWSERBASE_API_KEY", val); err != nil {
		return errf(5, "write %s: %v", path, err)
	}
	fmt.Printf("set BROWSERBASE_API_KEY in %s\n", path)
	printEnvApplyHint(false)
	fmt.Println("note: this key mints live-view links. Each user ALSO adds Browserbase")
	fmt.Println("      in Settings -> Connections to drive the browser — use a key from")
	fmt.Println("      the SAME Browserbase project, or minting fails for live sessions.")
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

// envKeyPattern is the shape of a variable name the server's env parser (and
// systemd's EnvironmentFile=) will actually read back.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// configSetEnv upserts ONE arbitrary KEY in an env file with the same guarded
// write the credential-specific verbs use — the generic seam that was missing.
// Hand-editing the 0600 file is how a duplicate OPENX_API_KEY line (last one
// wins at load) took a deployment down; this path dedupes to exactly one line,
// keeps the file 0600 and its owner unchanged (creds.SetEnvKey), and never
// puts the value on argv: it is read from stdin (`--value -`, or a pipe) or a
// hidden TTY prompt. --web targets the web-tier file instead.
func configSetEnv(argv []string) int {
	fs := flag.NewFlagSet("config set-env", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "env file (default: the server env file — FLEET_ENV_FILE, /etc/fleet/fleet.env when present, else .env.local)")
	web := fs.Bool("web", false, "write the web-tier env file instead (/etc/fleet/fleet-web.env when present, else web/.env.local)")
	value := fs.String("value", "", `the value ("-" reads stdin; omit to prompt hidden, or pipe it in)`)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fleet config set-env <KEY> [--value -] [--web] [--env-file <path>]")
		fmt.Fprintln(fs.Output(), "  Upserts KEY=VALUE as exactly one line (duplicates removed), file kept 0600 with its owner unchanged.")
		_, _ = io.WriteString(fs.Output(), "  The value never goes on argv: printf '%s' \"$V\" | fleet config set-env KEY   (or a hidden prompt on a TTY)\n")
		fs.PrintDefaults()
	}
	key, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errf(1, "usage: fleet config set-env <KEY> [--value -] [--web] [--env-file <path>]")
	}
	if !envKeyPattern.MatchString(key) {
		return errf(1, "invalid env key %q (want [A-Za-z_][A-Za-z0-9_]*)", key)
	}
	val, err := resolveSecret(*value, key+": ", false)
	if err != nil {
		return errf(1, "%v", err)
	}
	if strings.ContainsAny(val, "\r\n") {
		return errf(1, "value contains a line break — an env file cannot hold it (use fleet config unset-env to remove a key)")
	}
	path := serverEnvFile(*envFile)
	if *web {
		path = webEnvFile(*envFile)
	}
	if err := creds.SetEnvKey(path, key, val); err != nil {
		return errf(5, "write %s: %v", path, err)
	}
	fmt.Printf("set %s in %s (one line; file 0600, owner unchanged)\n", key, path)
	printEnvApplyHint(*web)
	return 0
}

// configUnsetEnv removes every line for KEY from the env file.
func configUnsetEnv(argv []string) int {
	fs := flag.NewFlagSet("config unset-env", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "env file (default: the server env file)")
	web := fs.Bool("web", false, "edit the web-tier env file instead")
	key, flagArgs := splitPositionalValueFlags(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	key = strings.TrimSpace(key)
	if key == "" || !envKeyPattern.MatchString(key) {
		return errf(1, "usage: fleet config unset-env <KEY> [--web] [--env-file <path>]")
	}
	path := serverEnvFile(*envFile)
	if *web {
		path = webEnvFile(*envFile)
	}
	removed, err := creds.DeleteEnvKey(path, key)
	if err != nil {
		return errf(5, "write %s: %v", path, err)
	}
	if !removed {
		fmt.Printf("%s was not set in %s\n", key, path)
		return 0
	}
	fmt.Printf("removed %s from %s\n", key, path)
	printEnvApplyHint(*web)
	return 0
}

// printEnvApplyHint is the one apply-instructions footer every env-file writer
// prints (config set-*, set-env/unset-env, mcp account set/del): preflight,
// then restart. One function so the writers cannot drift on what "apply" means.
func printEnvApplyHint(web bool) {
	if web {
		fmt.Println("apply with: systemctl restart fleet-web")
		return
	}
	fmt.Println("preflight with: fleet validate-config")
	fmt.Println("apply with: fleet restart   (systemctl restart fleet; reloadable knobs also apply live via kill -USR2 — docs/CONFIG-RELOAD.md)")
}
