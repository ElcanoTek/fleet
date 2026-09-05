package admincli

import (
	"fmt"
	"io"
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

// chatDSNFromFlags resolves the chat DB DSN: --database-url, else
// FLEET_CHAT_DATABASE_URL, else DATABASE_URL.
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
	return "", fmt.Errorf("chat DB DSN unset — pass --database-url or set FLEET_CHAT_DATABASE_URL / DATABASE_URL (in the shell or in %s)", serverEnvFile(""))
}

// schedDSN resolves the sched DB DSN: --database-url, else
// FLEET_SCHED_DATABASE_URL, else DATABASE_URL.
func schedDSN(dbURL string) (string, error) {
	if v := strings.TrimSpace(dbURL); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envOrFile("FLEET_SCHED_DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(envOrFile("DATABASE_URL")); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("sched DB DSN unset — pass --database-url or set FLEET_SCHED_DATABASE_URL / DATABASE_URL (in the shell or in %s)", serverEnvFile(""))
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
	envFileOnce.Do(func() {
		if vals, err := creds.ReadEnvValues(serverEnvFile("")); err == nil {
			envFileValues = vals
		}
	})
	return envFileValues[key]
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
