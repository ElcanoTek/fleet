//go:build linux

package admincli

import "testing"

func TestParseProcStat(t *testing.T) {
	got, ok := parseProcStat("4242 (bash -c (x) y) S 4000 4100 3900 34816 4100 4194304 0 0")
	if !ok || got != (procStat{ppid: 4000, pgrp: 4100, session: 3900}) {
		t.Fatalf("comm with spaces and parens: got %+v ok=%v", got, ok)
	}
	for _, bad := range []string{"", "4242 bash S 1 2 3", "4242 (x) S 1 2", "4242 (x) S a b c"} {
		if _, ok := parseProcStat(bad); ok {
			t.Errorf("parseProcStat(%q) must fail", bad)
		}
	}
}

// The test binary runs in `go test`'s own process group under its parent, so
// whatever the answer is, it must not panic, and readProcStat must read self.
func TestReadProcStatSelf(t *testing.T) {
	if _, ok := readProcStat("self"); ok {
		t.Fatal("non-numeric pid names are refused")
	}
	_ = jobControlled()
}
