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

// ReadEnvValues reads the env file at path and returns the values for the
// requested keys (only keys actually present in the file appear in the result;
// pass no keys to read every line). It mirrors the SERVER's env-file value
// handling (internal/config.loadEnvFile): an `export ` prefix is tolerated, an
// unquoted value's trailing ` #comment` is trimmed, and a single layer of
// surrounding quotes is stripped — so a value read here is byte-for-byte the
// value the server loads from the same file (this matters for the shared
// token). Values are returned to the caller but NEVER logged here. A missing
// file yields an empty map and no error (so "no file" and "key absent" look the
// same to callers); an unreadable file — e.g. a 0600 file the caller lacks
// permission for — returns the error so the caller can fall back.
func ReadEnvValues(path string, keys ...string) (map[string]string, error) {
	lines, err := readEnvLines(path)
	if err != nil {
		return nil, err
	}
	var want map[string]struct{}
	if len(keys) > 0 {
		want = make(map[string]struct{}, len(keys))
		for _, k := range keys {
			want[k] = struct{}{}
		}
	}
	out := make(map[string]string)
	for _, ln := range lines {
		k, v, ok := splitEnvKeyValue(ln)
		if !ok {
			continue
		}
		if want != nil {
			if _, requested := want[k]; !requested {
				continue
			}
		}
		out[k] = v
	}
	return out, nil
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
	k, _, ok := splitEnvKeyValue(line)
	return k, ok
}

// splitEnvKeyValue parses a KEY=VALUE line into its key and processed value,
// skipping blanks and `#` comments. An `export ` prefix is tolerated, the key
// is trimmed, and the value is run through the same trimming the server applies
// (inline-comment strip on unquoted values, then a single layer of surrounding
// quotes). ok is false for non-assignment lines.
func splitEnvKeyValue(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	t = strings.TrimPrefix(t, "export ")
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(t[:eq])
	value = stripEnvQuotes(stripEnvInlineComment(strings.TrimSpace(t[eq+1:])))
	return key, value, true
}

// stripEnvInlineComment trims a trailing ` #comment` off an unquoted value;
// quoted values are left intact. Mirrors internal/config.stripInlineComment.
func stripEnvInlineComment(value string) string {
	if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, `'`) {
		return value
	}
	if i := strings.Index(value, " #"); i >= 0 {
		return strings.TrimSpace(value[:i])
	}
	if i := strings.Index(value, "\t#"); i >= 0 {
		return strings.TrimSpace(value[:i])
	}
	return value
}

// stripEnvQuotes removes a single layer of matching surrounding quotes.
// Mirrors internal/config.stripQuotes.
func stripEnvQuotes(value string) string {
	if len(value) >= 2 &&
		((strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) ||
			(strings.HasPrefix(value, `'`) && strings.HasSuffix(value, `'`))) {
		return value[1 : len(value)-1]
	}
	return value
}
