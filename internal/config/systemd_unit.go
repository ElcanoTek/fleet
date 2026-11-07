package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// The fleet systemd unit's own environment: what the SERVICE process starts
// with on a systemd box. `fleet config explain` merges it in as a source that
// outranks the operator's shell (the shell is not the service), and `fleet
// doctor` / `fleet config migrate-env` use it to tell whether the env file is
// exported (EnvironmentFile=). The operator CLI asks `systemctl show` first
// (UnitEnvFromSystemctl) and falls back to parsing the unit files the way
// systemd resolves them (UnitEnvFromFiles) when systemctl is unavailable.

// SourceUnit names the systemd unit as a value's source.
const SourceUnit = "systemd unit"

// SystemdUnitDirs is systemd's system unit search path, highest priority
// first (systemd.unit(5)).
var SystemdUnitDirs = []string{
	"/etc/systemd/system",
	"/run/systemd/system",
	"/usr/local/lib/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
}

// UnitEnv is a unit's environment as systemd builds it: Environment=
// assignments, then each EnvironmentFile= in order (later files, and files
// over Environment=, win), then UnsetEnvironment= removals.
type UnitEnv struct {
	// Unit is the unit name (fleet.service).
	Unit string `json:"unit"`
	// Env is the resulting variable → value map.
	Env map[string]string `json:"-"`
	// Origin says where each Env value was set: "Environment=" or
	// "EnvironmentFile=<path>".
	Origin map[string]string `json:"-"`
	// Unset lists the names UnsetEnvironment= removes.
	Unset map[string]bool `json:"-"`
	// Files are the EnvironmentFile= paths, in order.
	Files []string `json:"environment_files"`
	// Unreadable lists EnvironmentFile= paths the CLI could not read (run as
	// root to include them).
	Unreadable []string `json:"unreadable_environment_files,omitempty"`
}

// unitDirectives is the raw [Service] list state before files are read.
type unitDirectives struct {
	environment []string // KEY=VALUE assignments
	files       []string
	unset       []string
}

// buildUnitEnv reads the EnvironmentFile= files and applies the lists.
func buildUnitEnv(unit string, d unitDirectives, readFile func(string) ([]byte, error)) *UnitEnv {
	u := &UnitEnv{Unit: unit, Env: map[string]string{}, Origin: map[string]string{}, Unset: map[string]bool{}, Files: d.files}
	for _, a := range d.environment {
		if k, v, ok := strings.Cut(a, "="); ok && k != "" {
			u.Env[k], u.Origin[k] = v, "Environment="
		}
	}
	for _, f := range d.files {
		b, err := readFile(f)
		if err != nil {
			if !os.IsNotExist(err) {
				u.Unreadable = append(u.Unreadable, f)
			}
			continue
		}
		for _, ln := range strings.Split(string(b), "\n") {
			if k, v, ok := creds.ParseEnvLine(ln); ok {
				u.Env[k], u.Origin[k] = v, "EnvironmentFile="+f
			}
		}
	}
	for _, n := range d.unset {
		// UnsetEnvironment= also takes KEY=VALUE (unset only on a match);
		// fleet's unit uses bare names, which is all this models.
		if !strings.Contains(n, "=") {
			u.Unset[n] = true
			delete(u.Env, n)
			delete(u.Origin, n)
		}
	}
	return u
}

// UnitEnvFromSystemctl builds the unit's environment from `systemctl show
// -p LoadState -p Environment -p EnvironmentFiles -p UnsetEnvironment <unit>`
// output. It returns nil when the unit is not loaded (LoadState=not-found).
func UnitEnvFromSystemctl(unit, show string, readFile func(string) ([]byte, error)) *UnitEnv {
	var d unitDirectives
	for _, ln := range strings.Split(show, "\n") {
		key, val, ok := strings.Cut(ln, "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			if strings.TrimSpace(val) == "not-found" {
				return nil
			}
		case "Environment":
			d.environment = append(d.environment, splitUnitWords(val)...)
		case "EnvironmentFiles": // one line per file: "<path> (ignore_errors=yes)"
			if f := strings.Fields(val); len(f) > 0 {
				d.files = append(d.files, strings.TrimPrefix(f[0], "-"))
			}
		case "UnsetEnvironment":
			d.unset = append(d.unset, splitUnitWords(val)...)
		}
	}
	return buildUnitEnv(unit, d, readFile)
}

// UnitEnvFromFiles builds the unit's environment from unit files the way
// systemd resolves them, for a box without a working systemctl:
//
//   - the base unit is the FIRST existing <dir>/<unit> in dirs (search order);
//   - drop-ins are <dir>/<unit>.d/*.conf from every dir; a drop-in in a
//     higher-priority dir masks one of the same file name in a lower one, and
//     the survivors apply in file-name order after the base unit;
//   - in [Service], Environment=, EnvironmentFile= and UnsetEnvironment=
//     append to their lists (EnvironmentFile's leading `-` dropped), and an
//     EMPTY assignment resets that list.
//
// It returns nil when no base unit exists.
func UnitEnvFromFiles(dirs []string, unit string, readFile func(string) ([]byte, error)) *UnitEnv {
	d, found := unitDirectivesFromFiles(dirs, unit)
	if !found {
		return nil
	}
	return buildUnitEnv(unit, d, readFile)
}

// UnitEnvironmentFiles is UnitEnvFromFiles' EnvironmentFile= list alone.
func UnitEnvironmentFiles(dirs []string, unit string) (files []string, found bool) {
	d, found := unitDirectivesFromFiles(dirs, unit)
	return d.files, found
}

func unitDirectivesFromFiles(dirs []string, unit string) (unitDirectives, bool) {
	var base string
	for _, dir := range dirs {
		p := filepath.Join(dir, unit)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			base = p
			break
		}
	}
	if base == "" {
		return unitDirectives{}, false
	}
	dropins := map[string]string{} // file name → path (highest priority wins)
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, unit+".d", "*.conf"))
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
	var d unitDirectives
	applyUnitFile(&d, base)
	for _, n := range names {
		applyUnitFile(&d, dropins[n])
	}
	return d, true
}

// applyUnitFile folds one unit file's [Service] environment directives into d.
func applyUnitFile(d *unitDirectives, path string) {
	b, err := os.ReadFile(path) // #nosec G304 — a systemd unit path under the search dirs
	if err != nil {
		return
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
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		words := splitUnitWords(val)
		switch strings.TrimSpace(key) {
		case "Environment":
			if val == "" {
				d.environment = nil
				continue
			}
			d.environment = append(d.environment, words...)
		case "EnvironmentFile":
			if val == "" {
				d.files = nil
				continue
			}
			for _, f := range words {
				d.files = append(d.files, strings.TrimPrefix(f, "-"))
			}
		case "UnsetEnvironment":
			if val == "" {
				d.unset = nil
				continue
			}
			d.unset = append(d.unset, words...)
		}
	}
}

// splitUnitWords splits a systemd value into words the way systemd does for
// Environment=: whitespace-separated, with "double" or 'single' quotes
// grouping a word and backslash escaping the next character.
func splitUnitWords(s string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote rune
		esc   bool
		in    bool
	)
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc, in = false, true
		case r == '\\':
			esc, in = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
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
