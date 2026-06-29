package tools

import (
	"strings"
	"testing"
)

func TestMaybeAddEmptyOutputHint(t *testing.T) {
	cases := []struct {
		name     string
		resp     pythonResponse
		wantHint bool
	}{
		{
			name:     "all empty success gets hint",
			resp:     pythonResponse{Status: "success"},
			wantHint: true,
		},
		{
			name:     "whitespace-only stdout still counts as empty",
			resp:     pythonResponse{Status: "success", Output: "\n", Stdout: "\n"},
			wantHint: true,
		},
		{
			name:     "stdout present",
			resp:     pythonResponse{Status: "success", Output: "42", Stdout: "42"},
			wantHint: false,
		},
		{
			name:     "vars returned",
			resp:     pythonResponse{Status: "success", Vars: map[string]any{"payload": "abc"}},
			wantHint: false,
		},
		{
			name:     "stderr carries its own signal",
			resp:     pythonResponse{Status: "success", Stderr: "warning: deprecated"},
			wantHint: false,
		},
		{
			name:     "error carries its own signal",
			resp:     pythonResponse{Status: "error", Error: "NameError: x"},
			wantHint: false,
		},
		{
			name:     "rendered figure counts as a trace",
			resp:     pythonResponse{Status: "success", ImageFiles: []string{"figures/fig-abc.png"}},
			wantHint: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maybeAddEmptyOutputHint(&tc.resp)
			if got := tc.resp.Hint != ""; got != tc.wantHint {
				t.Errorf("hint set = %v, want %v (hint=%q)", got, tc.wantHint, tc.resp.Hint)
			}
			if tc.wantHint && !strings.Contains(tc.resp.Hint, "NO output") {
				t.Errorf("hint text unexpected: %q", tc.resp.Hint)
			}
		})
	}
}
