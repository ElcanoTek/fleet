package config

import (
	"bufio"
	"os"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// The ONE implementation of how a setting's value is resolved (#2053, Codex
// round 2 on #2057): config.Load and `fleet config explain` both go through
// it, so explain cannot report a value the server would not use.
//
// Resolution has two steps:
//
//  1. Merge (envLayers.get): per variable NAME, a value present in the
//     process environment wins — even when it is empty — and only otherwise
//     does the env file's value count, and only when the name is admitted
//     (fleet's own loader: the allowlist; a systemd unit that exports the
//     file: every name). applyEnvFile writes exactly this merge into the
//     process environment at boot.
//  2. Pick (resolveSpellings): across a setting's spellings in resolution
//     order (canonical, then aliases), the FIRST one with a non-empty value
//     wins; an empty value counts as unset and the next spelling is tried.
//     lookupFleet, getenvDefault and the knob parser read the merged
//     environment this way.

// Value sources.
const (
	SourceProcessEnv = "process env"
	SourceEnvFile    = "env file"
	SourceDefault    = "default"
)

// envLayers is the two places a variable can come from.
type envLayers struct {
	// process looks a name up in the process environment (present-but-empty
	// counts as present).
	process func(string) (string, bool)
	// file holds the env file's values, last assignment winning.
	file map[string]string
	// admit reports whether a file value may reach the process environment.
	admit func(string) bool
}

// get returns name's value once the env file is applied to the process
// environment, and where it came from. present is false when neither layer
// supplies it. inFileButMasked / inFileNotAdmitted explain a file value that
// does not count.
func (l envLayers) get(name string) (value, source string, present bool, why fileMiss) {
	fv, inFile := l.file[name]
	if v, ok := l.process(name); ok {
		if inFile && fv != v {
			why = fileMasked
		}
		return creds.StripEnvQuotes(v), SourceProcessEnv, true, why
	}
	if !inFile {
		return "", "", false, fileAbsent
	}
	if l.admit != nil && !l.admit(name) {
		return "", "", false, fileNotAdmitted
	}
	return fv, SourceEnvFile, true, fileAbsent
}

// fileMiss says why an env-file value did not become the variable's value.
type fileMiss int

const (
	fileAbsent      fileMiss = iota // no file value, or it counted
	fileMasked                      // the process env has the name (possibly empty), which wins
	fileNotAdmitted                 // the loader does not admit the name from the file
)

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
