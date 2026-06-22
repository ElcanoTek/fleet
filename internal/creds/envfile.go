package creds

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Env-file editing for the `fleet mcp account` CLI. Secrets live at rest in the
// 0600 env file (e.g. .env.local) as KEY=VALUE lines; an account secret is just
// a suffixed key <VAR>_<ACCOUNT>. These helpers do a read-modify-write that
// preserves unrelated lines and comments, sets the file mode to 0600, and never
// echoes values (the CLI reads values from stdin, never argv).

// SetEnvKey upserts key=value in the env file at path, creating the file (0600)
// if it does not exist. An existing key's line is replaced in place; a new key
// is appended. Comments and unrelated lines are preserved.
func SetEnvKey(path, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key")
	}
	lines, err := readEnvLines(path)
	if err != nil {
		return err
	}
	newLine := key + "=" + value
	replaced := false
	for i, ln := range lines {
		if k, ok := splitEnvLine(ln); ok && k == key {
			lines[i] = newLine
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, newLine)
	}
	return writeEnvLines(path, lines)
}

// DeleteEnvKey removes key from the env file. Returns true if a line was
// removed. A missing file is not an error (nothing to delete).
func DeleteEnvKey(path, key string) (bool, error) {
	key = strings.TrimSpace(key)
	lines, err := readEnvLines(path)
	if err != nil {
		return false, err
	}
	out := make([]string, 0, len(lines))
	removed := false
	for _, ln := range lines {
		if k, ok := splitEnvLine(ln); ok && k == key {
			removed = true
			continue
		}
		out = append(out, ln)
	}
	if !removed {
		return false, nil
	}
	return removed, writeEnvLines(path, out)
}

// ListEnvKeys returns the KEYS defined in the env file (sorted), NEVER values.
// Used by `fleet mcp account list` to show which account seats are provisioned
// without ever printing a secret.
func ListEnvKeys(path string) ([]string, error) {
	lines, err := readEnvLines(path)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, ln := range lines {
		if k, ok := splitEnvLine(ln); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// readEnvLines reads the env file's lines. A missing file yields an empty slice
// (the caller may be creating it).
func readEnvLines(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-supplied env file
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// writeEnvLines writes the lines back atomically (temp + rename) with 0600 mode
// so the secret file is never world-readable.
func writeEnvLines(path string, lines []string) error {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		// Releasing the fd before the deferred os.Remove discards the temp file;
		// the close error is irrelevant since we're already returning err.
		_ = tmp.Close()
		return err
	}
	w := bufio.NewWriter(tmp)
	for _, ln := range lines {
		if _, err := w.WriteString(ln + "\n"); err != nil {
			_ = tmp.Close() // discarding the temp file; original error already being returned.
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close() // discarding the temp file; original error already being returned.
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// splitEnvLine returns (key, true) for a KEY=VALUE line, skipping blanks and
// comments. The key is trimmed; an `export ` prefix is tolerated.
func splitEnvLine(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", false
	}
	t = strings.TrimPrefix(t, "export ")
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return "", false
	}
	return strings.TrimSpace(t[:eq]), true
}
