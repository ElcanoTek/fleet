package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// `fleet config explain` (#2053). The contract: for each registered setting,
// explain reports its CONFIGURED value and that value's source, drawn from
// the sources explain can see —
//
//   - the systemd unit's own environment, on a systemd box (Environment=,
//     EnvironmentFile=, minus UnsetEnvironment=): what the service starts with;
//   - the process environment of the shell running explain;
//   - the env file, as fleet's own loader reads it;
//   - the setting's alias spellings (and, as a note only, a CHAT_/CUTLASS_
//     spelling ADR-0106 removed, which nothing reads);
//   - its default, including one inherited from another setting.
//
// It is not the running server's in-memory configuration: admin overrides
// made in the app, values a running process has not reloaded, and anything
// computed at run time are outside it. It resolves through the same code as
// config.Load (resolve.go) and changes nothing; TestExplainMatchesLoad and
// TestInheritedDefaultsMatchTheReaders pin it against the real readers.

// Redacted replaces every secret's value in an Explanation.
const Redacted = "<redacted>"

// ExplainOptions selects what explain reports and from which sources.
type ExplainOptions struct {
	// EnvFile is the deployment env file ("" for none).
	EnvFile string
	// Keys are canonical names or alias spellings; empty means "every
	// setting that is set or carries a note" (or every setting, with All).
	Keys []string
	All  bool
	// Unit is the systemd unit's environment (UnitEnvFromSystemctl /
	// UnitEnvFromFiles); nil when not on a systemd box or with --no-unit.
	Unit *UnitEnv
}

// ExplainSources says which sources the report drew from.
type ExplainSources struct {
	// ProcessEnv is always the environment of the shell running explain.
	ProcessEnv string `json:"process_env"`
	// EnvFile is the env file path read.
	EnvFile string `json:"env_file"`
	// EnvFileReadByLoader: fleet's own loader reads EnvFile (without a unit:
	// assumed, as `fleet serve` with FLEET_ENV_FILE does; with a unit: only
	// when the unit's FLEET_ENV_FILE names it). When false, the file's lines
	// reach the service only through the unit's EnvironmentFile=, if at all.
	EnvFileReadByLoader bool `json:"env_file_read_by_fleet_loader"`
	// SystemdUnit is the unit whose environment was merged in (nil: none).
	SystemdUnit *UnitEnv `json:"systemd_unit,omitempty"`
}

// ExplainReport is explain's result.
type ExplainReport struct {
	Sources  ExplainSources `json:"sources"`
	Settings []Explanation  `json:"settings"`
}

// Explanation describes one setting's configured value.
type Explanation struct {
	// Key is the canonical name.
	Key string `json:"key"`
	// Value is the configured value (Redacted for a secret; a DSN without
	// its credentials). Unset, it is the default: inherited from another
	// setting (InheritedFrom), or the registry's documented Default.
	Value string `json:"configured_value"`
	// Set reports whether a spelling of this setting supplied the value.
	Set bool `json:"set"`
	// Source is SourceUnit, SourceProcessEnv, SourceEnvFile or SourceDefault.
	Source string `json:"source"`
	// SourceDetail qualifies a unit source: "Environment=" or
	// "EnvironmentFile=<path>".
	SourceDetail string `json:"source_detail,omitempty"`
	// Spelling is the variable name that supplied the value (the canonical
	// name or an alias); empty for a default.
	Spelling string `json:"spelling,omitempty"`
	// InheritedFrom is the chain of settings an inherited default came
	// through, nearest first (FLEET_MEMORY_GRAPH_MODEL ← FLEET_MEMORY_MODEL ←
	// FLEET_METADATA_MODEL ← FLEET_TITLE_MODEL).
	InheritedFrom []string `json:"inherited_from,omitempty"`
	// InheritedSource says where the inherited value was set ("" when it is
	// the root setting's own default).
	InheritedSource string `json:"inherited_source,omitempty"`
	// Secret marks a redacted setting.
	Secret bool `json:"secret"`
	// Notes carries anything else an operator should know.
	Notes []string `json:"notes,omitempty"`
}

// Explain resolves the requested settings from the sources in opts. An
// unknown key is an error naming it (a removed CHAT_/CUTLASS_ spelling is
// an error naming the FLEET_ key). A missing env file is not an error.
func Explain(opts ExplainOptions) (*ExplainReport, error) {
	rep := &ExplainReport{Sources: ExplainSources{
		ProcessEnv:  "the shell running fleet config explain",
		EnvFile:     opts.EnvFile,
		SystemdUnit: opts.Unit,
	}}
	var file map[string]string
	if opts.EnvFile != "" {
		vals, err := readEnvFileValues(opts.EnvFile)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read env file %s: %w", opts.EnvFile, err)
		}
		file = vals
	}
	rep.Sources.EnvFileReadByLoader = opts.EnvFile != "" && loaderReadsEnvFile(opts.Unit, opts.EnvFile)
	if !rep.Sources.EnvFileReadByLoader {
		file = nil
	}
	layers := envLayers{unit: opts.Unit, process: os.LookupEnv, file: file, admit: isAllowedEnvVar}

	var entries []envreg.Entry
	if len(opts.Keys) > 0 {
		var unknown []string
		seen := map[string]bool{}
		for _, k := range opts.Keys {
			e, ok := envreg.Find(strings.TrimSpace(k))
			if !ok {
				if c, retired := envreg.RetiredAlias(strings.TrimSpace(k)); retired {
					if c == "" {
						k += " (a removed setting: nothing reads it; " + envreg.RetiredFix(strings.TrimSpace(k)) + ")"
					} else {
						k += " (a removed alias of " + c + ", ADR-0106: nothing reads it; use " + c + ")"
					}
				}
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
	for i := range entries {
		x := explainEntry(&entries[i], layers)
		if len(opts.Keys) == 0 && !opts.All && !x.Set && len(x.Notes) == 0 {
			continue
		}
		rep.Settings = append(rep.Settings, x)
	}
	return rep, nil
}

// loaderReadsEnvFile reports whether fleet's own env-file loader reads
// envFile: always without a unit model; with one, only when the service's
// FLEET_ENV_FILE names it (the standard unit unsets FLEET_ENV_FILE and
// exports the file with EnvironmentFile= instead).
func loaderReadsEnvFile(unit *UnitEnv, envFile string) bool {
	if unit == nil {
		return true
	}
	v, ok := unit.Env["FLEET_ENV_FILE"]
	return ok && filepath.Clean(strings.TrimSpace(v)) == filepath.Clean(envFile)
}

// describeLayer renders a layer source for a note or InheritedSource.
func describeLayer(spelling string, lv layerValue) string {
	s := lv.source
	if lv.detail != "" {
		s += " (" + lv.detail + ")"
	}
	if spelling != "" {
		s += " as " + spelling
	}
	return s
}

func explainEntry(e *envreg.Entry, layers envLayers) Explanation {
	x := Explanation{Key: e.Name, Secret: e.Secret}
	sv := resolveSetting(*e, layers.get)
	x.Value = sv.value
	switch {
	case sv.set:
		x.Set, x.Source, x.SourceDetail, x.Spelling = true, sv.layer.source, sv.layer.detail, sv.spelling
		if sv.layer.shellDiffers {
			x.Notes = append(x.Notes, fmt.Sprintf("your shell sets %s to a different value, but the service uses the systemd unit's", sv.spelling))
		}
	default:
		x.Source = SourceDefault
		x.InheritedFrom = sv.inheritedFrom
		if sv.rootSpelling != "" {
			x.InheritedSource = describeLayer(sv.rootSpelling, sv.rootLayer)
		}
	}
	if !sv.prose {
		x.Value = displayValue(e, x.Value)
	}

	// Notes: every other spelling that is set, and why it does not count.
	for _, name := range envreg.RetiredAliasesOf(e.Name) {
		if where := setSomewhere(layers, name); where != "" {
			x.Notes = append(x.Notes, fmt.Sprintf("%s is set (%s), but it is a removed alias (ADR-0106) and nothing reads it: rename it to %s", name, where, e.Name))
		}
	}
	for _, name := range e.Spellings() {
		lv := layers.get(name)
		if name == x.Spelling {
			if lv.miss == fileMasked {
				x.Notes = append(x.Notes, fmt.Sprintf("%s is also in the env file, but the %s value wins", name, lv.source))
			}
			continue
		}
		switch {
		case lv.unsetByUnit:
			if where := setSomewhere(envLayers{process: layers.process, file: layers.file}, name); where != "" {
				x.Notes = append(x.Notes, fmt.Sprintf("%s is set in the %s, but the systemd unit removes it (UnsetEnvironment=)", name, where))
			}
		case lv.miss == fileMasked:
			x.Notes = append(x.Notes, fmt.Sprintf("%s is set in the env file, but the %s also sets it (to %s), which wins", name, lv.source, emptyOr(lv.value)))
		case lv.miss == fileNotAdmitted:
			x.Notes = append(x.Notes, fmt.Sprintf("%s is set in the env file, which fleet's own loader does not admit — ignored (set it in the process environment or the unit)", name))
		case lv.present && lv.value != "" && x.Set:
			x.Notes = append(x.Notes, fmt.Sprintf("%s is also set (%s) but %s takes precedence", name, describeLayer("", lv), x.Spelling))
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

// setSomewhere names where name is set (non-empty) in any source, admitted
// or not.
func setSomewhere(l envLayers, name string) string {
	if l.unit != nil {
		if v := l.unit.Env[name]; v != "" {
			return SourceUnit
		}
	}
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
