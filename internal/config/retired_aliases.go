package config

import (
	"fmt"
	"log/slog"
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
// The settings removed outright (the internal service token, ADR-0108) are
// reported the same way, with what replaced them. envreg.RetiredAlias
// decides what counts; envreg.RetiredFix words the advice.

// RetiredAliasFinding is one removed spelling found set (non-empty).
type RetiredAliasFinding struct {
	Name      string // the removed spelling
	Canonical string // the FLEET_ name to set instead; "" for a setting removed outright
	Where     string // SourceProcessEnv, SourceEnvFile or SourceUnit (+ origin)
}

// String is the one-line warning. It never contains a value.
func (f RetiredAliasFinding) String() string {
	if f.Canonical == "" {
		return fmt.Sprintf("%s is set in the %s, but fleet no longer reads it: %s", f.Name, f.Where, envreg.RetiredFix(f.Name))
	}
	return fmt.Sprintf("%s is set in the %s, but fleet no longer reads it (the CHAT_/CUTLASS_ aliases were removed, ADR-0106): rename it to %s", f.Name, f.Where, f.Canonical)
}

// RetiredAliasFindings lists every removed alias spelling set (non-empty) in
// the process environment, the systemd unit's own environment (unit, nil
// when not modelled) or envFile — ONE finding per spelling, sorted by name.
// Under systemd every env-file line is also in the process environment (and
// the unit's), so a name found in several places is reported where the
// operator fixes it: the env file first, then the unit, then the process env.
// A missing env file is not an error; an unreadable one is.
func RetiredAliasFindings(envFile string, unit *UnitEnv) ([]RetiredAliasFinding, error) {
	var file map[string]string
	if envFile != "" {
		vals, err := creds.ReadEnvValues(envFile)
		if err != nil {
			return nil, fmt.Errorf("read env file %s: %w", envFile, err)
		}
		file = vals
	}
	found := map[string]RetiredAliasFinding{}
	// add records name at where unless an earlier (higher-priority) source
	// already did; sources are visited env file → unit → process env.
	add := func(name, value, where string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if _, dup := found[name]; dup {
			return
		}
		if c, ok := envreg.RetiredAlias(name); ok {
			found[name] = RetiredAliasFinding{Name: name, Canonical: c, Where: where}
		}
	}
	for k, v := range file {
		add(k, v, SourceEnvFile)
	}
	if unit != nil {
		for k, v := range unit.Env {
			origin := unit.Origin[k]
			where := SourceUnit + " (" + origin + ")"
			if envFile != "" && filepath.Clean(strings.TrimPrefix(origin, "EnvironmentFile=")) == filepath.Clean(envFile) {
				where = SourceEnvFile
			}
			add(k, v, where)
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			add(k, v, SourceProcessEnv)
		}
	}
	out := make([]RetiredAliasFinding, 0, len(found))
	for _, f := range found {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// warnRetiredEnvAliases logs one warning per removed alias spelling set in
// the process environment or envFile. Load calls it once at boot; it never
// fails boot and never logs a value.
func warnRetiredEnvAliases(envFile string) {
	findings, err := RetiredAliasFindings(envFile, nil)
	if err != nil {
		slog.Warn("config: could not check for removed env settings", "err", err)
		return
	}
	for _, f := range findings {
		slog.Warn("config: " + f.String())
	}
}
