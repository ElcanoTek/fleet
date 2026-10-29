package admincli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// readStdinValue reads a secret/value from stdin (used when a flag is "-"),
// trimming a single trailing newline. Keeps secrets off argv (and out of the
// process table / shell history).
func readStdinValue() (string, error) {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// promptHidden reads one secret from an interactive terminal with echo off,
// printing the prompt (and the trailing newline the disabled echo swallows) to
// stderr so it never pollutes stdout/pipes.
func promptHidden(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", strings.TrimSpace(prompt), err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// resolveSecret resolves a secret from, in order: an explicit --flag value of
// "-" (read from stdin, keeping it off argv), a non-empty --flag value (honored
// for scripts, though argv exposure is discouraged), or — when the flag is empty
// — an interactive hidden prompt on a TTY (falling back to stdin when piped).
// When confirm is set and we prompted interactively, the value is entered twice
// and must match, catching typos on create paths. isTTY is injected so tests can
// exercise the piped path deterministically.
func resolveSecret(flagVal, prompt string, confirm bool) (string, error) {
	switch {
	case flagVal == "-":
		return readStdinValue()
	case flagVal != "":
		return flagVal, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		// No flag and not a TTY (e.g. piped without "-") — read stdin anyway so
		// `printf '%s' pw | fleet admin add x` still works.
		return readStdinValue()
	}
	v, err := promptHidden(prompt)
	if err != nil {
		return "", err
	}
	if confirm {
		again, err := promptHidden("confirm " + prompt)
		if err != nil {
			return "", err
		}
		if again != v {
			return "", fmt.Errorf("entries did not match")
		}
	}
	return v, nil
}

// dbPartsDSN constructs a Postgres URL from DB_HOST/DB_PORT/DB_USER/DB_PASSWORD/
// DB_NAME/DB_SSLMODE when at least one of those keys is set in the process
// environment or the deployment env file (or unconditionally when always=true),
// matching config.buildDatabaseURL and sched/db.Database.Init.
func dbPartsDSN(defaultUser, defaultName string, always bool) string {
	host := strings.TrimSpace(envOrFile("DB_HOST"))
	port := strings.TrimSpace(envOrFile("DB_PORT"))
	user := strings.TrimSpace(envOrFile("DB_USER"))
	pass := envOrFile("DB_PASSWORD")
	name := strings.TrimSpace(envOrFile("DB_NAME"))
	ssl := strings.TrimSpace(envOrFile("DB_SSLMODE"))
	if !always && host == "" && port == "" && user == "" && pass == "" && name == "" && ssl == "" {
		return ""
	}
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "5432"
	}
	if user == "" {
		user = defaultUser
	}
	if name == "" {
		name = defaultName
	}
	if ssl == "" {
		ssl = "disable"
	}
	auth := url.User(user).String()
	if pass != "" {
		auth = url.UserPassword(user, pass).String()
	}
	return fmt.Sprintf("postgres://%s@%s:%s/%s?sslmode=%s", auth, host, port, name, ssl)
}

// chatDSNFromFlags resolves the chat DB DSN: --database-url, else
// FLEET_CHAT_DATABASE_URL, else DATABASE_URL, else DB_* parts.
func chatDSN(dbURL string) (string, error) {
	if v := strings.TrimSpace(dbURL); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envOrFile("FLEET_CHAT_DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envOrFile("DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := dbPartsDSN("chat", "chat", false); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("chat DB DSN unset — pass --database-url or set FLEET_CHAT_DATABASE_URL / DATABASE_URL (in the shell or in %s)", serverEnvFile(""))
}

// schedDSN resolves the sched DB DSN: --database-url, else
// FLEET_SCHED_DATABASE_URL, else SCHED_DATABASE_URL, else DATABASE_URL, else DB_* parts.
func schedDSN(dbURL string) (string, error) {
	if v := strings.TrimSpace(dbURL); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envOrFile("FLEET_SCHED_DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envOrFile("SCHED_DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envOrFile("DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := dbPartsDSN("fleet", "sched", false); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("sched DB DSN unset — pass --database-url or set FLEET_SCHED_DATABASE_URL / DATABASE_URL (in the shell or in %s)", serverEnvFile(""))
}

// idleChatDSN resolves the chat DB DSN including the unconditional localhost:5432
// default used by config.buildDatabaseURL at runtime.
func idleChatDSN(dbURL string) string {
	if v, err := chatDSN(dbURL); err == nil && v != "" {
		return v
	}
	return dbPartsDSN("chat", "chat", true)
}

// idleSchedDSN resolves the sched DB DSN including the unconditional localhost:5432
// default used by sched/db.Database.Init at runtime.
func idleSchedDSN(dbURL string) string {
	if v, err := schedDSN(dbURL); err == nil && v != "" {
		return v
	}
	return dbPartsDSN("fleet", "sched", true)
}

// envOrFile reads key from the process environment, falling back to the
// deployment's server env file (serverEnvFile: FLEET_ENV_FILE, else
// /etc/fleet/fleet.env on a provisioned box, else .env.local). The shipped
// unit reads that file itself and deliberately does not export it to login
// shells, so from a fresh root shell `fleet status` reported a healthy box as
// "✗ OPENROUTER_API_KEY unset / DSN unresolved" and every DB-backed verb
// demanded --database-url — the same fallback keystore.go already applies for
// FLEET_DATA_DIR. The process env always wins so an operator override still
// works; the file is read once per process (tests re-arm the read with
// resetEnvFileCache). Every deployment-owned knob the CLI consults —
// FLEET_CLIENT_CONFIG_DIR, the sandbox image, FLEET_BACKUP_DIR, the admin key
// and orchestrator address for `mcp reload` — goes through here rather than a
// bare os.Getenv, so no verb can contradict `fleet status` about what the
// deployment is configured with.
func envOrFile(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	loadEnvFile()
	return envFileValues[key]
}

// loadEnvFile reads the server env file once. A file that is missing or
// unreadable (root-owned 0600, the CLI run as another user) leaves only the
// shell environment.
func loadEnvFile() {
	envFileOnce.Do(func() {
		if vals, err := creds.ReadEnvValues(serverEnvFile("")); err == nil {
			envFileValues = vals
		}
	})
}

var (
	envFileOnce   sync.Once
	envFileValues map[string]string
)

// resetEnvFileCache re-arms envOrFile's once-per-process env-file read so
// tests can point FLEET_ENV_FILE at a fresh fixture.
func resetEnvFileCache() {
	envFileOnce = sync.Once{}
	envFileValues = nil
}

// errf prints to stderr and returns the given exit code.
func errf(code int, format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	return code
}

// splitPositionalValueFlags lifts the first true positional out of argv for
// flag sets where EVERY flag takes a value. Unlike splitPositional (which
// treats any non-dash token as the positional), a dash token without an
// embedded "=" is assumed to consume the NEXT token as its value — so
// `--from key.txt` keeps key.txt bound to --from instead of misreading it as
// the positional. Only safe for flag sets with no boolean flags.
func splitPositionalValueFlags(argv []string) (first string, flagArgs []string) {
	i := 0
	for i < len(argv) {
		a := argv[i]
		if len(a) > 0 && a[0] == '-' {
			flagArgs = append(flagArgs, a)
			// "--flag=value" is self-contained; bare "--flag" consumes the next
			// token as its value.
			if !strings.Contains(a, "=") && i+1 < len(argv) {
				flagArgs = append(flagArgs, argv[i+1])
				i += 2
				continue
			}
			i++
			continue
		}
		if first == "" {
			first = a
		} else {
			flagArgs = append(flagArgs, a)
		}
		i++
	}
	return first, flagArgs
}

// splitPositionalMixed lifts the first true positional out of argv for flag
// sets that MIX boolean and value flags. A dash token naming a flag in
// boolFlags (bare name, no dashes — e.g. "dry-run") never consumes the next
// token; any other dash token without an embedded "=" consumes the following
// token as its value. This is what lets
// `fleet import --sched-database-url X bundle.json` and
// `fleet import bundle.json --dry-run` both bind bundle.json as the positional
// (#714): splitPositional would misread a value flag's argument as the
// positional, and splitPositionalValueFlags would let a boolean flag swallow
// the bundle path.
func splitPositionalMixed(argv []string, boolFlags map[string]bool) (first string, flagArgs []string) {
	i := 0
	for i < len(argv) {
		a := argv[i]
		if len(a) > 1 && a[0] == '-' {
			flagArgs = append(flagArgs, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") {
				i++ // "--flag=value" is self-contained
				continue
			}
			if !boolFlags[name] && i+1 < len(argv) {
				flagArgs = append(flagArgs, argv[i+1]) // value flag: bind the next token
				i += 2
				continue
			}
			i++
			continue
		}
		if first == "" {
			first = a
		} else {
			flagArgs = append(flagArgs, a)
		}
		i++
	}
	return first, flagArgs
}
