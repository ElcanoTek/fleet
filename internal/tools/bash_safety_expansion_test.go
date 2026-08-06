package tools

import "testing"

// checkCommandSafety had no test coverage, and its ${...} branch rejected any
// command containing ":-", ":+", ":?", "##" or "%%" — tested against the WHOLE
// command rather than the inside of the expansion. A production run was refused
// on `echo "${CUTLASS_INPUT_DIR:-not set}"`, which is the ordinary way to read a
// variable with a default and what every `set -u` script needs. None of those
// forms can execute anything; the execution vectors inside an expansion are
// command substitution and backticks, and those are matched independently in the
// same loop. These cases pin both halves: the ordinary forms pass, and every
// construct that can actually run something still does not.
func TestCheckCommandSafetyParameterExpansion(t *testing.T) {
	allowed := []struct {
		name string
		cmd  string
	}{
		{"default when unset", `echo "${CUTLASS_INPUT_DIR:-not set}"`},
		{"empty default for set -u", `set -u; echo "${MAYBE:-}"`},
		{"alternate value", `echo "${DEBUG:+--verbose}"`},
		{"strip longest prefix", `echo "${PATHISH##*/}"`},
		{"strip longest suffix", `echo "${FILE%%.*}"`},
		{"pattern substitution", `echo "${NAME/foo/bar}"`},
		{"braced reference", `echo "${HOME}/work"`},
		{"bare reference", `echo $HOME`},
		{"length", `echo "${#LIST}"`},
		// The ":-" is not in an expansion at all here; the old whole-command
		// substring test still rejected it because a ${...} appeared elsewhere.
		{"unrelated colon-dash on the line", `echo "${HOME}" && printf 'a:-b\n'`},
	}
	for _, tc := range allowed {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			if err := checkCommandSafety(tc.cmd); err != nil {
				t.Errorf("checkCommandSafety(%q) = %v, want nil", tc.cmd, err)
			}
		})
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		// Name-level indirection: which variable is read is itself hidden in a
		// variable. The one form on the old list that earns its place.
		{"indirect expansion", `echo "${!SECRET_NAME}"`},
		{"indirect prefix glob", `echo "${!AWS_*}"`},
		// Still caught by the patterns that come after "${" in the same loop —
		// falling through with continue must not lose them.
		{"substitution inside a default", `echo "${MISSING:-$(id)}"`},
		{"backtick inside a default", "echo \"${MISSING:-`id`}\""},
		{"bare command substitution", `echo $(id)`},
		{"bare backticks", "echo `id`"},
		{"eval", `eval "echo hi"`},
		{"ansi-c quoting", `echo $'\x41'`},
	}
	for _, tc := range blocked {
		t.Run("block/"+tc.name, func(t *testing.T) {
			if err := checkCommandSafety(tc.cmd); err == nil {
				t.Errorf("checkCommandSafety(%q) = nil, want an error", tc.cmd)
			}
		})
	}
}
