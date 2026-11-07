package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// `fleet config explain` (#2053): for each registered setting, the effective
// value, where it came from, and whether that spelling is deprecated. It
// resolves through the same two functions config.Load does (resolve.go:
// envLayers.get for the env-file merge, resolveSpellings for the spelling
// chain), without loading anything; TestExplainMatchesLoad pins the two
// against each other.

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
	// file that does not count, another spelling that is also set, an alias
	// only some readers honour.
	Notes []string `json:"notes,omitempty"`
}

// explainLayers builds the merge explain shares with applyEnvFile. exported
// (the box's systemd unit loads the file with EnvironmentFile=) admits every
// name, since systemd exports every line into the server's environment.
func explainLayers(file map[string]string, exported bool) envLayers {
	admit := isAllowedEnvVar
	if exported {
		admit = func(string) bool { return true }
	}
	return envLayers{process: os.LookupEnv, file: file, admit: admit}
}

// Explain resolves the named settings (canonical names or alias spellings)
// against the process environment and envFile. With no keys it returns every
// setting that is set or carries a note (an alias the server ignores, a file
// value that does not count); with all, every registered setting. An unknown
// key is an error naming it. A missing env file is not an error.
//
// exported says the box's systemd unit loads envFile with EnvironmentFile=
// (SystemdExportsEnvFile), so every line of it reaches the server even when
// fleet's own env-file loader would not admit the name.
func Explain(envFile string, keys []string, all, exported bool) ([]Explanation, error) {
	file := map[string]string{}
	if envFile != "" {
		vals, err := readEnvFileValues(envFile)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read env file %s: %w", envFile, err)
		}
		if vals != nil {
			file = vals
		}
	}
	layers := explainLayers(file, exported)

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
		x := explainEntry(&entries[i], layers, exported)
		if len(keys) == 0 && !all && !x.Set && len(x.Notes) == 0 {
			continue
		}
		out = append(out, x)
	}
	return out, nil
}

// serverSpellings are the spellings the server resolves for e, in order: all
// of them except reader-specific aliases (Alias.ReadBy), which only one other
// reader honours.
func serverSpellings(e *envreg.Entry) []string {
	out := make([]string, 0, 1+len(e.Aliases))
	out = append(out, e.Name)
	for _, a := range e.Aliases {
		if a.ReadBy == "" {
			out = append(out, a.Name)
		}
	}
	return out
}

func explainEntry(e *envreg.Entry, layers envLayers, exported bool) Explanation {
	x := Explanation{Key: e.Name, Secret: e.Secret, Source: SourceDefault, Value: e.Default}

	// The value: the shared spelling chain over the shared merge.
	sources := map[string]string{}
	get := func(name string) (string, bool) {
		v, src, present, _ := layers.get(name)
		sources[name] = src
		return v, present
	}
	if v, spelling, ok := resolveSpellings(serverSpellings(e), get); ok {
		x.Set, x.Value, x.Source, x.Spelling = true, v, sources[spelling], spelling
		if a, _, isAlias := envreg.AliasOf(spelling); isAlias {
			x.Deprecated = a.Deprecated
		}
		if x.Source == SourceEnvFile && exported && !isAllowedEnvVar(spelling) {
			x.Notes = append(x.Notes, fmt.Sprintf("%s reaches the server only through the systemd unit's EnvironmentFile=; fleet's own env-file loader does not admit this spelling", spelling))
		}
		x.Value = displayValue(e, x.Value)
	}

	// The notes: every other spelling that is set, and why it does not count.
	for _, a := range e.Aliases {
		if a.ReadBy == "" {
			continue
		}
		if where := setSomewhere(layers, a.Name); where != "" {
			x.Notes = append(x.Notes, fmt.Sprintf("set as %s (%s), which only %s reads; the server ignores it", a.Name, where, a.ReadBy))
		}
	}
	for _, name := range serverSpellings(e) {
		if name == x.Spelling {
			continue
		}
		v, src, present, why := layers.get(name)
		switch {
		case why == fileMasked:
			x.Notes = append(x.Notes, fmt.Sprintf("%s is set in the env file, but the process env also sets it (to %s), which wins", name, emptyOr(v)))
		case why == fileNotAdmitted:
			x.Notes = append(x.Notes, fmt.Sprintf("%s is set in the env file, which fleet's own loader does not admit — ignored unless the systemd unit exports the file (set it in the process environment)", name))
		case present && v != "" && x.Set:
			x.Notes = append(x.Notes, fmt.Sprintf("%s is also set (%s) but %s takes precedence", name, src, x.Spelling))
		}
	}
	return x
}

// emptyOr names an empty value without printing a non-empty one.
func emptyOr(v string) string {
	if v == "" {
		return "an empty value"
	}
	return "a value"
}

// setSomewhere names where name is set (non-empty), admitted or not.
func setSomewhere(l envLayers, name string) string {
	if v, ok := l.process(name); ok && v != "" {
		return SourceProcessEnv
	}
	if l.file[name] != "" {
		return SourceEnvFile
	}
	return ""
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
