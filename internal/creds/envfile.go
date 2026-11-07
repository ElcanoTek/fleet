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
//
// The result always has EXACTLY ONE line for key. A hand-edited file can carry
// duplicates (one appended below another), and the server's loader is
// last-assignment-wins (config.loadEnvFileFiltered calls os.Setenv per line),
// so an upsert that replaced only the first occurrence left the stale later
// line in force — the value the operator just set was silently ignored. The
// first occurrence keeps its position; every later duplicate is dropped.
//
// The value is encoded so the shared parser (ParseEnvLine, which the server's
// config loader also uses) reads back the exact bytes given: values
// the parser would mangle (surrounding quotes, leading/trailing whitespace,
// an inline ` #` sequence) are wrapped in one layer of quotes, which the
// parser strips without touching the interior. A value containing a line
// break is refused — a line-oriented KEY=VALUE file cannot represent it, and
// writing it raw would corrupt the file (#834).
func SetEnvKey(path, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key")
	}
	encoded, err := encodeEnvValue(value)
	if err != nil {
		return fmt.Errorf("key %s: %w", key, err)
	}
	lines, err := readEnvLines(path)
	if err != nil {
		return err
	}
	newLine := key + "=" + encoded
	out := make([]string, 0, len(lines)+1)
	replaced := false
	for _, ln := range lines {
		if k, ok := splitEnvLine(ln); ok && k == key {
			if replaced {
				continue // a later duplicate: drop it, or it would win at load time
			}
			out = append(out, newLine)
			replaced = true
			continue
		}
		out = append(out, ln)
	}
	if !replaced {
		out = append(out, newLine)
	}
	return writeEnvLines(path, out)
}

// EnvLine renders one KEY=VALUE line exactly as SetEnvKey writes it (the value
// encoded so ParseEnvLine reads back the same bytes; a line break refused).
func EnvLine(key, value string) (string, error) {
	encoded, err := encodeEnvValue(value)
	if err != nil {
		return "", fmt.Errorf("key %s: %w", key, err)
	}
	return key + "=" + encoded, nil
}

// EditEnvLines rewrites the env file at path in ONE atomic write: edit gets
// its current lines and returns the new ones, which replace the file the way
// SetEnvKey's do (0600, owner kept, fsync before rename). For changes that
// must land together or not at all, such as moving a setting from one key
// to another; an edit error leaves the file untouched.
func EditEnvLines(path string, edit func([]string) ([]string, error)) error {
	lines, err := readEnvLines(path)
	if err != nil {
		return err
	}
	out, err := edit(lines)
	if err != nil {
		return err
	}
	return writeEnvLines(path, out)
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
		k, v, ok := ParseEnvLine(ln)
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

// ReadEnvLines returns the env file's raw lines, comments and blanks
// included, for a caller that rewrites the file while preserving its layout
// (`fleet config migrate-env`). A missing file yields no lines and no error.
func ReadEnvLines(path string) ([]string, error) { return readEnvLines(path) }

// WriteEnvLines writes lines back with the same guarantees as every writer
// here: atomic (temp + fsync + rename), mode 0600, owner preserved.
func WriteEnvLines(path string, lines []string) error { return writeEnvLines(path, lines) }

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
// so the secret file is never world-readable. When the file already exists its
// owner is preserved across the rename (PreserveOwner): a root-run
// `fleet config set-*` / `fleet mcp account set` must not re-own an env file to
// root out from under the account that owned it.
func writeEnvLines(path string, lines []string) error {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	prev, statErr := os.Stat(path) // nil when the file is being created
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
	// fsync BEFORE the rename (#1124): the rename atomically swaps the
	// directory entry, but without a Sync the new file's data may still be
	// dirty page cache — a power loss shortly after `fleet mcp account set`
	// could then leave the credentials file empty/truncated on some
	// filesystems, which for a secrets file means locked-out connectors.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // discarding the temp file; original error already being returned.
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if statErr == nil {
		if err := PreserveOwner(tmpName, prev); err != nil {
			return err
		}
	}
	return os.Rename(tmpName, path)
}

// splitEnvLine returns (key, true) for a KEY=VALUE line, skipping blanks and
// comments. The key is trimmed; an `export ` prefix is tolerated.
func splitEnvLine(line string) (string, bool) {
	k, _, ok := ParseEnvLine(line)
	return k, ok
}

// ParseEnvLine parses a KEY=VALUE env-file line into its key and processed
// value, skipping blanks and `#` comments. It is the ONE env-file line parser:
// the server's config loader and this package's reader/writer both use it, so
// the encoding encodeEnvValue writes is exactly what the server reads (#834).
//
//   - an `export ` prefix is tolerated, and the key is trimmed;
//   - an UNQUOTED value ends at the first ` #` or `\t#` (inline comment) — a
//     value that must contain a literal ` #` MUST be quoted, or it is
//     truncated at the marker (SetEnvKey quotes such values automatically);
//   - ONE layer of matching surrounding quotes (single or double) is stripped;
//     the interior — including ` #`, edge whitespace and inner quotes — is
//     preserved verbatim.
//
// ok is false for non-assignment lines.
func ParseEnvLine(line string) (key, value string, ok bool) {
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
	value = StripEnvQuotes(stripEnvInlineComment(strings.TrimSpace(t[eq+1:])))
	return key, value, true
}

// stripEnvInlineComment trims a trailing ` #comment` off an unquoted value;
// quoted values are left intact (StripEnvQuotes owns those).
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

// encodeEnvValue returns the on-disk form of value such that every reader of
// the file yields the original bytes: ParseEnvLine (which the server's
// config loader also uses), systemd's EnvironmentFile= (deploy/fleet.service
// reads /etc/fleet/fleet.env — that is what the running service sees), and the
// scripts' env_get. scripts/lib/envfile.sh's fleet_env_encode applies the same
// rules to what bootstrap.sh and update.sh write.
//
// The readers disagree on everything but plain text and one layer of quotes:
// systemd unescapes backslashes (unquoted and inside double quotes) and keeps an
// unquoted ` #…`, while fleet's parser has no escapes and cuts an unquoted value
// at ` #`. So:
//   - a plain value (printable non-space edges; no quote, backslash, `$`,
//     backtick, leading `#` or whitespace-`#`) is written verbatim;
//   - otherwise, a value without `'` is wrapped in single quotes, which every
//     reader takes literally;
//   - otherwise, a value without `"` or `\` is wrapped in double quotes, where
//     systemd has nothing to unescape;
//   - anything else (a `'` together with a `"` or `\`), and any line break,
//     has no form all readers agree on and is refused rather than written in a
//     form the service would read differently (#834, #1838).
func encodeEnvValue(value string) (string, error) {
	if strings.ContainsAny(value, "\n\r") {
		return "", fmt.Errorf("value contains a line break, which a KEY=VALUE env file cannot represent")
	}
	switch {
	case envValueVerbatim(value):
		return value, nil
	case !strings.Contains(value, "'"):
		return "'" + value + "'", nil
	case !strings.ContainsAny(value, `"\`):
		return `"` + value + `"`, nil
	}
	return "", fmt.Errorf("value contains a ' together with a \" or \\, which no env-file quoting reads back the same under systemd and fleet's own parser")
}

// envValueVerbatim reports whether value reads back unchanged, unquoted, under
// every reader (see encodeEnvValue).
func envValueVerbatim(value string) bool {
	if value == "" {
		return true
	}
	printable := func(c byte) bool { return c >= '!' && c <= '~' }
	if !printable(value[0]) || !printable(value[len(value)-1]) || value[0] == '#' {
		return false
	}
	if strings.ContainsAny(value, "'\"\\$`") {
		return false
	}
	for i := 1; i < len(value); i++ {
		if value[i] == '#' && strings.IndexByte(" \t\v\f", value[i-1]) >= 0 {
			return false
		}
	}
	return true
}

// StripEnvQuotes removes a single layer of matching surrounding single or
// double quotes. It is also how the server unquotes a value that reached the
// process environment some other way (systemd, a container runtime), so a
// quoted value reads the same whichever path delivered it.
func StripEnvQuotes(value string) string {
	if len(value) >= 2 &&
		((strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) ||
			(strings.HasPrefix(value, `'`) && strings.HasSuffix(value, `'`))) {
		return value[1 : len(value)-1]
	}
	return value
}
