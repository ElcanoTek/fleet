package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Which env files does the fleet systemd unit export (EnvironmentFile=)? On
// the standard install it exports the deployment env file, which makes every
// line of that file part of the server's environment — including spellings
// fleet's own env-file loader does not admit. `fleet config explain` and
// `fleet doctor` need the answer; the operator CLI asks `systemctl show -p
// EnvironmentFiles` first and falls back to UnitEnvironmentFiles when
// systemctl is unavailable.

// SystemdUnitDirs is systemd's system unit search path, highest priority
// first (systemd.unit(5)).
var SystemdUnitDirs = []string{
	"/etc/systemd/system",
	"/run/systemd/system",
	"/usr/local/lib/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
}

// UnitEnvironmentFiles resolves unit's EnvironmentFile= list the way systemd
// does, from unit files on disk:
//
//   - the base unit is the FIRST existing <dir>/<unit> in dirs (search order);
//   - drop-ins are <dir>/<unit>.d/*.conf from every dir; a drop-in in a
//     higher-priority dir masks one of the same file name in a lower one, and
//     the survivors apply in file-name order after the base unit;
//   - each EnvironmentFile= in [Service] appends its paths (a leading `-`,
//     "ignore if missing", is dropped); an EMPTY assignment resets the list.
//
// found is false when no base unit exists.
func UnitEnvironmentFiles(dirs []string, unit string) (files []string, found bool) {
	var base string
	for _, d := range dirs {
		p := filepath.Join(d, unit)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			base = p
			break
		}
	}
	if base == "" {
		return nil, false
	}
	dropins := map[string]string{} // file name → path (highest priority wins)
	for _, d := range dirs {
		matches, _ := filepath.Glob(filepath.Join(d, unit+".d", "*.conf"))
		for _, m := range matches {
			if _, seen := dropins[filepath.Base(m)]; !seen {
				dropins[filepath.Base(m)] = m
			}
		}
	}
	names := make([]string, 0, len(dropins))
	for n := range dropins {
		names = append(names, n)
	}
	sort.Strings(names)
	files = applyEnvironmentFileLines(files, base)
	for _, n := range names {
		files = applyEnvironmentFileLines(files, dropins[n])
	}
	return files, true
}

// applyEnvironmentFileLines folds one unit file's [Service] EnvironmentFile=
// assignments into files.
func applyEnvironmentFileLines(files []string, path string) []string {
	b, err := os.ReadFile(path) // #nosec G304 — a systemd unit path under the search dirs
	if err != nil {
		return files
	}
	section := ""
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, ";"):
			continue
		case strings.HasPrefix(ln, "[") && strings.HasSuffix(ln, "]"):
			section = ln
			continue
		}
		if section != "[Service]" {
			continue
		}
		key, val, ok := strings.Cut(ln, "=")
		if !ok || strings.TrimSpace(key) != "EnvironmentFile" {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			files = nil // an empty assignment resets the list
			continue
		}
		for _, f := range strings.Fields(val) {
			files = append(files, strings.TrimPrefix(f, "-"))
		}
	}
	return files
}

// ParseSystemctlEnvironmentFiles parses `systemctl show -p EnvironmentFiles
// --value <unit>`: one entry per line, "<path> (ignore_errors=yes|no)".
func ParseSystemctlEnvironmentFiles(out string) []string {
	var files []string
	for _, ln := range strings.Split(out, "\n") {
		if f := strings.Fields(ln); len(f) > 0 {
			files = append(files, strings.TrimPrefix(f[0], "-"))
		}
	}
	return files
}

// EnvFileIn reports whether envFile is one of files (compared cleaned).
func EnvFileIn(envFile string, files []string) bool {
	want := filepath.Clean(envFile)
	for _, f := range files {
		if filepath.Clean(f) == want {
			return true
		}
	}
	return false
}
