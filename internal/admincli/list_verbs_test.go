package admincli

import "testing"

// TestListVerbsRefuseStrayPositionals — every `list` verb takes no positional,
// so a stray argument is a usage error (exit 1) rather than silently discarded:
// `fleet chat user list alice` used to print every user and exit 0 as if the
// name had filtered something. The check runs before any DSN is resolved or DB
// opened, which is what makes it testable here (and what makes the old
// behaviour a foot-gun: the verb went to the database on a command line it
// had not understood). `sched apikey list` set the pattern.
func TestListVerbsRefuseStrayPositionals(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) int
	}{
		{"chat user list", chatUserList},
		{"notes list", notesList},
		{"sched user list", schedUserList},
		{"admin list", adminList},
		{"sched dlq list", schedDLQList},
		{"sched apikey list", schedAPIKeyList},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := tc.run([]string{"stray"}); code != 1 {
				t.Errorf("%s stray: exit %d, want 1", tc.name, code)
			}
			// A stray token AFTER a flag is caught too (flag parsing stops at
			// the first non-flag, leaving it in fs.Args()).
			if code := tc.run([]string{"--json", "stray"}); code != 1 {
				t.Errorf("%s --json stray: exit %d, want 1", tc.name, code)
			}
			// And a typo'd flag is a parse error, not ignored.
			if code := tc.run([]string{"--josn"}); code != 1 {
				t.Errorf("%s --josn: exit %d, want 1", tc.name, code)
			}
		})
	}
}
