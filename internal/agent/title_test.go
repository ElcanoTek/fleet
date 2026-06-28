package agent

import "testing"

func TestHeuristicTitle(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Stacked filler ("can you help me") is fully stripped; acronyms/mixed
		// case survive; connector words stay lowercase mid-title.
		{"can you help me debug this Python function?", "Debug This Python Function"},
		{"what is the best way to sort a list in Go?", "The Best Way to Sort a List in Go"},
		{"summarize the last 3 emails from Alice", "Summarize the Last 3 Emails from Alice"},
		{"How do I configure TLS?", "Configure TLS"},
		{"please   refactor    the parser", "Refactor the Parser"},
		// Degenerate inputs fall back rather than producing junk.
		{"", "New conversation"},
		{"hi", "New conversation"},
		{"   ", "New conversation"},
	}
	for _, c := range cases {
		if got := HeuristicTitle(c.in); got != c.want {
			t.Errorf("HeuristicTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHeuristicTitle_Truncates(t *testing.T) {
	long := "explain the entire history of distributed consensus algorithms from Paxos to Raft and beyond in great detail"
	got := HeuristicTitle(long)
	if len([]rune(got)) > 50 {
		t.Errorf("title should truncate to ~50 chars, got %d: %q", len([]rune(got)), got)
	}
	if got == "New conversation" {
		t.Errorf("a long meaningful message should not fall back: %q", got)
	}
}
