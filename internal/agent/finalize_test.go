package agent

import (
	"testing"
)

// The force-summary prompt shape is now pinned end-to-end in
// interactive_test.go (TestInteractiveFinalize_ForceSummarySeesTurnWork): the
// old TestBuildForceSummaryMessages exercised a PriorHistory/TurnHistory replay
// that production never wired (#1117), which is exactly how the wrong-history
// bug survived a green suite.

func TestStripLeakedToolCalls(t *testing.T) {
	// The exact leak observed in the wild — a download_url call narrated as
	// text. Should collapse to empty so the forced-summary fallback fires.
	leak := "call:default_api:download_url{output_dir:/opt/chat/workspace/abc,url:https://api.fast.io/x/read/?token=eyJ0eXAiabc._sig}"
	if got := stripLeakedToolCalls(leak); got != "" {
		t.Errorf("leaked-only reply: got %q, want empty", got)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain prose unchanged", "Here is your report. Spend rose 12% WoW.", "Here is your report. Spend rose 12% WoW."},
		{"prose mentioning call: but not a leak", "I'll call: the publisher to confirm.", "I'll call: the publisher to confirm."},
		{
			"real answer with a stray leaked call inline",
			"Done — see the table.\ncall:default_api:download_url{url:https://x/y}\nLet me know if you need more.",
			"Done — see the table.\n\nLet me know if you need more.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripLeakedToolCalls(c.in); got != c.want {
				t.Errorf("stripLeakedToolCalls(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
