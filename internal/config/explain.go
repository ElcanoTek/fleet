package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// `fleet config explain` (#2053): for each registered setting, the effective
// value, where it came from, and whether that spelling is deprecated — read
// from the process environment and the deployment env file the way Load reads
// them, without loading anything.

// Value sources, as `fleet config explain` names them.
const (
	SourceProcessEnv = "process env"
	SourceEnvFile    = "env file"
	SourceDefault    = "default"
)

// Redacted replaces every secret's value in an Explanation.
const Redacted = "<redacted>"

// Explanation describes one setting's effective value.
type Explanation struct {
	// Key is the canonical name.
	Key string `json:"key"`
	// Value is the effective value (Redacted for a secret that is set; the
	// documented default when unset).
	Value string `json:"value"`
	// Set reports whether any spelling supplied a non-empty value.
	Set bool `json:"set"`
	// Source is SourceProcessEnv, SourceEnvFile or SourceDefault.
	Source string `json:"source"`
	// Spelling is the variable name that supplied the value (the canonical
	// name or an alias); empty for the default.
	Spelling string `json:"spelling,omitempty"`
	// Deprecated reports that Spelling is a deprecated legacy alias.
	Deprecated bool `json:"deprecated"`
	// Secret marks a redacted setting.
	Secret bool `json:"secret"`
	// Notes carries anything else an operator should know: a value in the env
	// file the loader ignores, another spelling that is also set, an alias
	// only some readers honour.
	Notes []string `json:"notes,omitempty"`
}

// envSnapshot is the two places a value can come from.
type envSnapshot struct {
	file map[string]string
	// exported: the deployment's systemd unit loads the env file with
	// EnvironmentFile=, so EVERY line of it reaches the server's process
	// environment — not only the names fleet's own loader admits.
	exported bool
}

// lookup resolves one spelling the way the server sees it: a process-env
// value wins; otherwise an env-file value counts when fleet's own loader
// admits the name, or when systemd exports the whole file. viaSystemd marks
// a value that only systemd delivers; ignored marks one nothing delivers.
func (s envSnapshot) lookup(name string) (value, source string, viaSystemd, ignored bool) {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return creds.StripEnvQuotes(v), SourceProcessEnv, false, false
	}
	v, inFile := s.file[name]
	if !inFile || v == "" {
		return "", "", false, false
	}
	if isAllowedEnvVar(name) {
		return v, SourceEnvFile, false, false
	}
	if s.exported {
		return v, SourceEnvFile, true, false
	}
	return "", "", false, true
}

// present reports whether name is set (non-empty) anywhere, admitted or not.
func (s envSnapshot) present(name string) (where string, ok bool) {
	if v, set := os.LookupEnv(name); set && v != "" {
		return SourceProcessEnv, true
	}
	if v := s.file[name]; v != "" {
		return SourceEnvFile, true
	}
	return "", false
}

// Explain resolves the named settings (canonical names or alias spellings)
// against the process environment and envFile. With no keys it returns every
// setting that is set; with all, every registered setting. An unknown key is
// an error naming it. A missing env file is not an error.
//
// exported says the box's systemd unit loads envFile with EnvironmentFile=
// (EnvFileExportedBySystemd), so every line of it reaches the server even
// when fleet's own env-file loader would not admit the name.
func Explain(envFile string, keys []string, all, exported bool) ([]Explanation, error) {
	file := map[string]string{}
	if envFile != "" {
		vals, err := creds.ReadEnvValues(envFile)
		if err != nil {
			return nil, fmt.Errorf("read env file %s: %w", envFile, err)
		}
		file = vals
	}
	snap := envSnapshot{file: file, exported: exported}

	var entries []envreg.Entry
	if len(keys) > 0 {
		var unknown []string
		seen := map[string]bool{}
		for _, k := range keys {
			e, ok := envreg.Find(strings.TrimSpace(k))
			if !ok {
				unknown = append(unknown, k)
				continue
			}
			if !seen[e.Name] {
				seen[e.Name] = true
				entries = append(entries, e)
			}
		}
		if len(unknown) > 0 {
			return nil, fmt.Errorf("not a registered setting: %s (see docs/CONFIGURATION.md)", strings.Join(unknown, ", "))
		}
	} else {
		entries = envreg.All()
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	}

	out := make([]Explanation, 0, len(entries))
	for i := range entries {
		x := explainEntry(&entries[i], snap)
		if len(keys) == 0 && !all && !x.Set {
			continue
		}
		out = append(out, x)
	}
	return out, nil
}

func explainEntry(e *envreg.Entry, snap envSnapshot) Explanation {
	x := Explanation{Key: e.Name, Secret: e.Secret, Source: SourceDefault, Value: e.Default}
	for _, name := range e.Spellings() {
		// A reader-specific alias (only one reader, never the server, honours
		// it) is never the server's effective value: report it, skip it.
		if a, _, isAlias := envreg.AliasOf(name); isAlias && a.ReadBy != "" {
			if where, ok := snap.present(name); ok {
				x.Notes = append(x.Notes, fmt.Sprintf("set as %s (%s), which only %s reads; the server ignores it", name, where, a.ReadBy))
			}
			continue
		}
		v, src, viaSystemd, ignored := snap.lookup(name)
		if ignored {
			x.Notes = append(x.Notes, fmt.Sprintf("%s is set in the env file, which fleet's own loader does not admit — ignored unless the systemd unit exports the file (set it in the process environment)", name))
			continue
		}
		if src == "" {
			continue
		}
		if x.Set {
			x.Notes = append(x.Notes, fmt.Sprintf("%s is also set (%s) but %s takes precedence", name, src, x.Spelling))
			continue
		}
		x.Set, x.Value, x.Source, x.Spelling = true, v, src, name
		if viaSystemd {
			x.Notes = append(x.Notes, fmt.Sprintf("%s reaches the server only through the systemd unit's EnvironmentFile=; fleet's own env-file loader does not admit this spelling", name))
		}
		if a, _, isAlias := envreg.AliasOf(name); isAlias {
			x.Deprecated = a.Deprecated
		}
	}
	if x.Set {
		x.Value = displayValue(e, x.Value)
	}
	return x
}

// displayValue is what explain may print for a set value: nothing of a
// secret (a DSN keeps its host and database, without credentials), and no
// credentials embedded in any other URL-shaped value (userinfo, token-like
// query parameters).
func displayValue(e *envreg.Entry, v string) string {
	switch {
	case e.Secret && e.Type == envreg.TypeDSN:
		return RedactDSN(v)
	case e.Secret:
		return Redacted
	case strings.Contains(v, "://"):
		return RedactDSN(v)
	default:
		return v
	}
}

// EnvFileExportedBySystemd reports whether any of unitFiles (a unit and its
// drop-ins) loads envFile with EnvironmentFile= — in which case systemd
// exports every line of the file into the server's environment, whatever
// fleet's own env-file loader admits. Unreadable files count as absent.
func EnvFileExportedBySystemd(envFile string, unitFiles []string) bool {
	want := filepath.Clean(envFile)
	for _, uf := range unitFiles {
		b, err := os.ReadFile(uf) // #nosec G304 — a systemd unit path the CLI derived
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(string(b), "\n") {
			ln = strings.TrimSpace(ln)
			val, ok := strings.CutPrefix(ln, "EnvironmentFile=")
			if !ok {
				continue
			}
			for _, f := range strings.Fields(val) {
				if filepath.Clean(strings.TrimPrefix(f, "-")) == want {
					return true
				}
			}
		}
	}
	return false
}
