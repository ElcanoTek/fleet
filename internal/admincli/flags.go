package admincli

import (
	"fmt"
	"io"
	"os"
	"strings"
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

// chatDSNFromFlags resolves the chat DB DSN: --database-url, else
// FLEET_CHAT_DATABASE_URL, else DATABASE_URL.
func chatDSN(dbURL string) (string, error) {
	if v := strings.TrimSpace(dbURL); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_CHAT_DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("DATABASE_URL")); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("chat DB DSN unset — pass --database-url or set FLEET_CHAT_DATABASE_URL / DATABASE_URL")
}

// schedDSN resolves the sched DB DSN: --database-url, else
// FLEET_SCHED_DATABASE_URL, else DATABASE_URL.
func schedDSN(dbURL string) (string, error) {
	if v := strings.TrimSpace(dbURL); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_SCHED_DATABASE_URL")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("DATABASE_URL")); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("sched DB DSN unset — pass --database-url or set FLEET_SCHED_DATABASE_URL / DATABASE_URL")
}

// envFilePath resolves the credential env file: --env-file, else
// FLEET_ENV_FILE, else .env.local.
func envFilePath(flag string) string {
	if v := strings.TrimSpace(flag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_ENV_FILE")); v != "" {
		return v
	}
	return ".env.local"
}

// errf prints to stderr and returns the given exit code.
func errf(code int, format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	return code
}
