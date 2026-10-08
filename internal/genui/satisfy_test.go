package genui

import "testing"

func TestNeverTrue(t *testing.T) {
	cases := map[string]bool{
		"gate && !gate":            true,
		"!(a && b) && a && b":      true,
		"(a || b) && !a && !b":     true,
		"a && false":               true,
		"gate":                     false,
		"gate || !gate":            false,
		"a > 1 && a < 0":           false, // value logic is not judged: never a false alarm
		"x == 1 && !(x == 1)":      true,  // the same comparison is the same atom
		"x == 1 && !(x==1)":        true,  // spacing does not matter
		"len(xs) > 0 && !gate":     false,
		"gate ? a : !a":            false,
		"rows.cpm && !rows.cpm":    true,
		"!empty(n) && empty(n)":    true,
		"-n && !(-n)":              true,
		"contains(tags, 'a') || b": false,
	}
	for src, want := range cases {
		if _, err := ParseExpr(src); err != nil {
			t.Fatalf("%q does not parse: %v", src, err)
		}
		if got := neverTrue(src); got != want {
			t.Errorf("neverTrue(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestNeverUsable(t *testing.T) {
	cases := []struct {
		vis, dis string
		want     bool
	}{
		{"gate", "gate", true},
		{"gate", "gate || block", true},
		{"", "gate || !gate", true},
		{"a && b", "a", true},
		{"gate && !gate", "", true},
		{"gate", "!gate || block", false},
		{"gate", "block", false},
		{"", "block", false},
		{"a || b", "a", false},
	}
	for _, c := range cases {
		if got := neverUsable(c.vis, c.dis); got != c.want {
			t.Errorf("neverUsable(%q, %q) = %v, want %v", c.vis, c.dis, got, c.want)
		}
	}
}
