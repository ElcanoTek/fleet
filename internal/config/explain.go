package config

import (
	"fmt"
	"os"
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
}

// lookup resolves one spelling the way applyEnvFile + the readers do: a
// process-env value wins; otherwise an env-file value counts only when the
// env file admits that name.
func (s envSnapshot) lookup(name string) (value, source string, ignored bool) {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return creds.StripEnvQuotes(v), SourceProcessEnv, false
	}
	v, inFile := s.file[name]
	if !inFile || v == "" {
		return "", "", false
	}
	if !isAllowedEnvVar(name) {
		return "", "", true
	}
	return v, SourceEnvFile, false
}

// Explain resolves the named settings (canonical names or alias spellings)
// against the process environment and envFile. With no keys it returns every
// setting that is set; with all, every registered setting. An unknown key is
// an error naming it. A missing env file is not an error.
func Explain(envFile string, keys []string, all bool) ([]Explanation, error) {
	file := map[string]string{}
	if envFile != "" {
		vals, err := creds.ReadEnvValues(envFile)
		if err != nil {
			return nil, fmt.Errorf("read env file %s: %w", envFile, err)
		}
		file = vals
	}
	snap := envSnapshot{file: file}

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
		v, src, ignored := snap.lookup(name)
		if ignored {
			x.Notes = append(x.Notes, fmt.Sprintf("%s is set in the env file, which does not admit it — ignored (set it in the process environment)", name))
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
		if a, _, isAlias := envreg.AliasOf(name); isAlias {
			x.Deprecated = a.Deprecated
			if a.Note != "" {
				x.Notes = append(x.Notes, name+": "+a.Note)
			}
		}
	}
	if x.Secret && x.Set {
		x.Value = Redacted
	}
	return x
}
