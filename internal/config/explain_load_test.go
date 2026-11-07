package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// spellingState is where one spelling of a setting is set for a case.
type spellingState int

const (
	stAbsent        spellingState = iota
	stProcess                     // process env, non-empty
	stProcessEmpty                // process env, present but empty
	stFile                        // env file only
	stProcessEmptyF               // process env empty AND the env file sets it
	numStates
)

// explainVsLoadCase is a setting explain resolves and the Config field Load
// resolves it into.
type explainVsLoadCase struct {
	key   string
	field func(*Config) string
	// valueFor turns a raw value into what the field holds (Load normalises
	// some settings, e.g. appending an extension).
	valueFor func(string) string
	// codeDefault is what Load yields when nothing is set.
	codeDefault string
}

// TestExplainMatchesLoad — Codex round 2 (#2057): explain must report the
// value config.Load actually uses. For each setting below, every combination
// of {absent, process env, process env present-but-empty, env file, process
// empty + env file} across ALL its spellings (canonical, CHAT_/CUTLASS_
// aliases, the bare PERSONA_DEFAULT, and the reader-specific CHAT_TLS_MODE /
// DATA_DIR the server ignores) is applied; explain runs first (it changes
// nothing), then Load, and the two must agree on the value and its source.
// Values are unique per spelling and layer, so agreement on the value proves
// agreement on which spelling and layer won.
func TestExplainMatchesLoad(t *testing.T) {
	chdir(t, t.TempDir())
	id := func(s string) string { return s }
	cases := []explainVsLoadCase{
		{key: "FLEET_SERVER_ADDR", field: func(c *Config) string { return c.Addr }, valueFor: id, codeDefault: "127.0.0.1:8080"},
		{key: "FLEET_TASK_MODEL", field: func(c *Config) string { return c.TaskModel }, valueFor: id, codeDefault: ""},
		{key: "FLEET_PERSONA_DEFAULT", field: func(c *Config) string { return c.PersonaDefault }, valueFor: id, codeDefault: "assistant"},
		{key: "FLEET_DATA_DIR", field: func(c *Config) string { return c.DataDir }, valueFor: id, codeDefault: "./data"},
		{key: "FLEET_TLS_MODE", field: func(c *Config) string { return c.TLSMode }, valueFor: strings.ToLower, codeDefault: "off"},
		{key: "SYSTEM_PROMPT", field: func(c *Config) string { return c.SystemPrompt }, valueFor: func(s string) string { return s + ".md" }, codeDefault: "default.md"},
	}
	total := 0
	for _, tc := range cases {
		e, ok := envreg.Find(tc.key)
		if !ok {
			t.Fatalf("%s is not registered", tc.key)
		}
		spellings := e.Spellings()
		combos := 1
		for range spellings {
			combos *= int(numStates)
		}
		for n := 0; n < combos; n++ {
			states := make([]spellingState, len(spellings))
			for i, rest := 0, n; i < len(spellings); i, rest = i+1, rest/int(numStates) {
				states[i] = spellingState(rest % int(numStates))
			}
			total++
			checkExplainAgainstLoad(t, tc, spellings, states)
		}
	}
	if total < 500 {
		t.Fatalf("only %d cases ran; the matrix is broken", total)
	}
}

func checkExplainAgainstLoad(t *testing.T, tc explainVsLoadCase, spellings []string, states []spellingState) {
	t.Helper()
	name := fmt.Sprintf("%s/%v", tc.key, states)
	t.Run(name, func(t *testing.T) {
		isolateEnv(t)
		unsetForTest(t, spellings...)
		var file []string
		for i, s := range spellings {
			switch states[i] {
			case stProcess:
				t.Setenv(s, "p-"+strings.ToLower(s))
			case stProcessEmpty:
				t.Setenv(s, "")
			case stFile:
				file = append(file, s+"=f-"+strings.ToLower(s))
			case stProcessEmptyF:
				t.Setenv(s, "")
				file = append(file, s+"=f-"+strings.ToLower(s))
			default: // stAbsent: nothing set
			}
		}
		sort.Strings(file)
		path := filepath.Join(t.TempDir(), "fleet.env")
		writeEnv(t, path, strings.Join(file, "\n")+"\n")

		xs, err := Explain(path, []string{tc.key}, false, false)
		if err != nil {
			t.Fatal(err)
		}
		x := xs[0]
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		got := tc.field(cfg)
		if !x.Set {
			if got != tc.codeDefault {
				t.Fatalf("explain says unset (default) but Load used %q; notes %v", got, x.Notes)
			}
			return
		}
		if want := tc.valueFor(x.Value); got != want {
			t.Fatalf("explain says %q from %s (%s) but Load used %q", x.Value, x.Spelling, x.Source, got)
		}
		wantSource := SourceEnvFile
		if strings.HasPrefix(x.Value, "p-") {
			wantSource = SourceProcessEnv
		}
		if x.Source != wantSource || !strings.HasSuffix(x.Value, strings.ToLower(x.Spelling)) {
			t.Fatalf("explain source %s / spelling %s does not match its value %q", x.Source, x.Spelling, x.Value)
		}
	})
}

// Codex round 2 (#2057) finding 1: a process variable that is present but
// EMPTY wins over the env file, exactly as Load applies it
// (TestLoadOverridesWithEmptyString) — explain must not report the file's
// value for that spelling.
func TestExplainEmptyProcessValueMasksTheFile(t *testing.T) {
	unsetForTest(t, "FLEET_SERVER_ADDR", "CHAT_SERVER_ADDR", "CUTLASS_SERVER_ADDR")
	t.Setenv("FLEET_SERVER_ADDR", "")
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "FLEET_SERVER_ADDR=10.0.0.1:9\n")
	xs, err := Explain(path, []string{"FLEET_SERVER_ADDR"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	x := xs[0]
	if x.Set || x.Value != "127.0.0.1:8080" {
		t.Fatalf("got %+v, want the default: the empty process value masks the file", x)
	}
	if !strings.Contains(strings.Join(x.Notes, "\n"), "the process env also sets it (to an empty value), which wins") {
		t.Errorf("notes %v do not explain the masked file value", x.Notes)
	}
}

// Codex round 2 (#2057) finding 2: with no KEY, explain lists every entry
// that carries a note even when it is not set — e.g. only CHAT_TLS_MODE,
// which the server ignores.
func TestExplainListsNoteOnlyEntries(t *testing.T) {
	unsetForTest(t, "FLEET_TLS_MODE", "CHAT_TLS_MODE", "CUTLASS_TLS_MODE")
	t.Setenv("CHAT_TLS_MODE", "auto")
	xs, err := Explain("", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range xs {
		if x.Key == "FLEET_TLS_MODE" {
			if x.Set || len(x.Notes) == 0 {
				t.Fatalf("FLEET_TLS_MODE = %+v, want unset with a note", x)
			}
			return
		}
	}
	t.Fatal("FLEET_TLS_MODE (only CHAT_TLS_MODE set) is missing from the default listing")
}
