package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// The CHAT_<X>/CUTLASS_<X> spellings of a FLEET_<X> setting (and the bare
// DATA_DIR) were removed (ADR-0106): nothing reads them, and the env-file
// loader does not admit them. An operator who still sets one would otherwise
// see the setting silently fall back to its default, so boot (Load) and
// `fleet doctor` report every one they find, with the FLEET_ name to use.
// envreg.RetiredAlias decides what counts.

// RetiredAliasFinding is one removed spelling found set (non-empty).
type RetiredAliasFinding struct {
	Name      string // the removed spelling
	Canonical string // the FLEET_ name to set instead
	Where     string // SourceProcessEnv, SourceEnvFile or SourceUnit (+ origin)
}

// String is the one-line warning. It never contains a value.
func (f RetiredAliasFinding) String() string {
	return fmt.Sprintf("%s is set in the %s, but fleet no longer reads it (the CHAT_/CUTLASS_ aliases were removed, ADR-0106): rename it to %s", f.Name, f.Where, f.Canonical)
}

// RetiredAliasFindings lists every removed alias spelling set (non-empty) in
// the process environment, the systemd unit's own environment (unit, nil
// when not modelled) or envFile, sorted by name then source. A line the unit
// takes from envFile itself is reported once, as the env-file finding. A
// missing env file is not an error; an unreadable one is.
func RetiredAliasFindings(envFile string, unit *UnitEnv) ([]RetiredAliasFinding, error) {
	var file map[string]string
	if envFile != "" {
		vals, err := creds.ReadEnvValues(envFile)
		if err != nil {
			return nil, fmt.Errorf("read env file %s: %w", envFile, err)
		}
		file = vals
	}
	var out []RetiredAliasFinding
	add := func(name, value, where string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if c, ok := envreg.RetiredAlias(name); ok {
			out = append(out, RetiredAliasFinding{Name: name, Canonical: c, Where: where})
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			add(k, v, SourceProcessEnv)
		}
	}
	if unit != nil {
		for k, v := range unit.Env {
			origin := unit.Origin[k]
			if envFile != "" && filepath.Clean(strings.TrimPrefix(origin, "EnvironmentFile=")) == filepath.Clean(envFile) {
				continue
			}
			add(k, v, SourceUnit+" ("+origin+")")
		}
	}
	for k, v := range file {
		add(k, v, SourceEnvFile)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Where < out[j].Where
	})
	return out, nil
}

// warnRetiredEnvAliases logs one warning per removed alias spelling set in
// the process environment or envFile. Load calls it once at boot; it never
// fails boot and never logs a value.
func warnRetiredEnvAliases(envFile string) {
	findings, err := RetiredAliasFindings(envFile, nil)
	if err != nil {
		log.Printf("Warning: could not check for removed CHAT_/CUTLASS_ env aliases: %v", err)
		return
	}
	for _, f := range findings {
		log.Printf("Warning: %s", f)
	}
}
