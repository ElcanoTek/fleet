package config

import (
	"bufio"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// The ONE implementation of how a setting's configured value is resolved
// (#2053; Codex rounds 2–3 on #2057). config.Load and `fleet config explain`
// both go through it, so explain cannot report a value the code would not
// use.
//
//  1. Merge, per variable NAME (envLayers.get). Highest first:
//     - the systemd unit's own environment (explain only, on a systemd box:
//     Environment=, EnvironmentFile=, minus UnsetEnvironment=);
//     - the process environment — present wins, even when EMPTY;
//     - the env file, when the loader admits the name.
//     applyEnvFile writes exactly the last two into the process environment
//     at boot (inside the service, the unit's environment IS the process
//     environment).
//  2. Pick, across a setting's spellings (resolveSpellings): the first
//     spelling with a non-empty value wins; empty counts as unset.
//  3. Default (resolveSetting): an unset setting with DefaultFrom inherits
//     its parent's resolved value (recursively, through these same steps),
//     else its Default.

// Value sources.
const (
	SourceProcessEnv = "process env"
	SourceEnvFile    = "env file"
	SourceDefault    = "default"
)

// envLayers is the places a variable can come from.
type envLayers struct {
	// unit is the systemd unit's environment (nil: not modelled).
	unit *UnitEnv
	// process looks a name up in the process environment (present-but-empty
	// counts as present).
	process func(string) (string, bool)
	// file holds the env file's values, last assignment winning; nil when
	// the loader does not read a file.
	file map[string]string
	// admit reports whether a file value may reach the process environment.
	admit func(string) bool
}

// fileMiss says why an env-file value did not become the variable's value.
type fileMiss int

const (
	fileAbsent      fileMiss = iota // no file value, or it counted
	fileMasked                      // a higher layer has the name (possibly empty), which wins
	fileNotAdmitted                 // the loader does not admit the name from the file
)

// layerValue is one variable's merged value and how it got it.
type layerValue struct {
	value, source string
	// detail qualifies a unit source: "Environment=" or "EnvironmentFile=<path>".
	detail  string
	present bool
	miss    fileMiss
	// shellDiffers: the unit sets the name and the shell sets it to
	// something else, which the service never sees.
	shellDiffers bool
	// unsetByUnit: UnsetEnvironment= removes the name from the service.
	unsetByUnit bool
}

// get merges one variable name across the layers.
func (l envLayers) get(name string) layerValue {
	fv, inFile := l.file[name]
	pv, inProcess := l.process(name)
	if l.unit != nil {
		if v, ok := l.unit.Env[name]; ok {
			lv := layerValue{value: v, source: SourceUnit, detail: l.unit.Origin[name], present: true,
				shellDiffers: inProcess && creds.StripEnvQuotes(pv) != v}
			if inFile && fv != v {
				lv.miss = fileMasked
			}
			return lv
		}
		if l.unit.Unset[name] {
			return layerValue{unsetByUnit: true}
		}
	}
	if inProcess {
		lv := layerValue{value: creds.StripEnvQuotes(pv), source: SourceProcessEnv, present: true}
		if inFile && fv != pv {
			lv.miss = fileMasked
		}
		return lv
	}
	if !inFile {
		return layerValue{}
	}
	if l.admit != nil && !l.admit(name) {
		return layerValue{miss: fileNotAdmitted}
	}
	return layerValue{value: fv, source: SourceEnvFile, present: true}
}

// resolveSpellings picks a setting's value from its spellings in resolution
// order: the first non-empty value wins.
func resolveSpellings(spellings []string, get func(string) (string, bool)) (value, spelling string, ok bool) {
	for _, s := range spellings {
		if v, present := get(s); present && v != "" {
			return v, s, true
		}
	}
	return "", "", false
}

// serverSpellings are the spellings the code resolves for e, in order: all
// of them except reader-specific aliases (Alias.ReadBy).
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

// settingValue is a setting's resolved configured value.
type settingValue struct {
	value    string
	set      bool   // a spelling of the setting itself supplied value
	spelling string // that spelling
	layer    layerValue
	// inheritedFrom is the DefaultFrom chain the default came through (the
	// nearest parent first); empty for a plain Default or a set value.
	inheritedFrom []string
	// root is where an inherited value was finally set (its spelling and
	// layer); zero when it came from a Default.
	rootSpelling string
	rootLayer    layerValue
	// prose: value is descriptive default text, not a value.
	prose bool
}

// resolveSetting resolves e: its own spellings (resolveSpellings over the
// merged layers), else its inherited default (DefaultFrom, resolved through
// this same function), else its Default.
func resolveSetting(e envreg.Entry, get func(string) layerValue) settingValue {
	layers := map[string]layerValue{}
	pick := func(name string) (string, bool) {
		lv := get(name)
		layers[name] = lv
		return lv.value, lv.present
	}
	if v, sp, ok := resolveSpellings(serverSpellings(&e), pick); ok {
		return settingValue{value: v, set: true, spelling: sp, layer: layers[sp]}
	}
	if e.DefaultFrom != "" {
		parent, _ := envreg.Find(e.DefaultFrom)
		var pv settingValue
		if e.DefaultFromCanonical {
			lv := get(parent.Name)
			if lv.present && lv.value != "" {
				pv = settingValue{value: lv.value, set: true, spelling: parent.Name, layer: lv}
			}
		} else {
			pv = resolveSetting(parent, get)
		}
		inherited := pv.value
		if pv.prose {
			inherited = ""
		}
		v, ok := inherited, inherited != ""
		if e.DefaultDerive != nil {
			v, ok = e.DefaultDerive(inherited)
		}
		if ok {
			out := settingValue{value: v, inheritedFrom: append([]string{parent.Name}, pv.inheritedFrom...)}
			if pv.set {
				out.rootSpelling, out.rootLayer = pv.spelling, pv.layer
			} else {
				out.rootSpelling, out.rootLayer = pv.rootSpelling, pv.rootLayer
			}
			return out
		}
	}
	return settingValue{value: e.Default, prose: strings.HasPrefix(e.Default, "(")}
}

// readEnvFileValues parses an env file into name → value, last assignment
// winning, exactly as the boot loader reads it (creds.ParseEnvLine, the
// default scanner buffer). It does not filter by the allowlist — that is
// envLayers.admit's job — and returns the os error for a missing file.
func readEnvFileValues(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 — path comes from trusted config.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := creds.ParseEnvLine(sc.Text()); ok {
			out[k] = v
		}
	}
	return out, sc.Err()
}
