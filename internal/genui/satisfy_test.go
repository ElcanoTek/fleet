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
		if got := neverTrue(src, nil); got != want {
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
		if got := neverUsable(c.vis, c.dis, nil); got != c.want {
			t.Errorf("neverUsable(%q, %q) = %v, want %v", c.vis, c.dis, got, c.want)
		}
	}
}

func TestBooleanComparisons(t *testing.T) {
	bools := map[string]string{"gate": kindBool}
	cases := []struct {
		vis, dis string
		want     bool
	}{
		{"gate == true", "gate", true},
		{"true == gate", "gate", true},
		{"gate != false", "gate", true},
		{"gate == false", "!gate", true},
		{"gate != true", "!gate", true},
		{"gate == true", "!gate", false},
		{"gate == false", "gate", false},
		// Not a toggle: the comparison stays opaque (no false alarm).
		{"name == true", "name", false},
	}
	for _, c := range cases {
		if got := neverUsable(c.vis, c.dis, bools); got != c.want {
			t.Errorf("neverUsable(%q, %q) = %v, want %v", c.vis, c.dis, got, c.want)
		}
	}
}

func TestNumberComparisons(t *testing.T) {
	kinds := map[string]string{"n": kindNumber, "s": kindSlider}
	cases := []struct {
		vis, dis string
		want     bool
	}{
		{"n > 0", "n >= 0", true},
		{"n > 5", "n > 3", true},
		{"0 < n", "n > 0", true},
		{"n == 3", "n > 2", true},
		{"n >= 10", "n < 0 || n > 5", true},
		{"n > 0", "n > 5", false},  // n = 3
		{"n < 0", "n >= 0", false}, // n = -1
		{"n != 3", "n > 2", false}, // blank: != holds, ordering reads 0
		{"!n", "n == 0", false},    // blank n is falsy, and blank never == 0
		{"s == 0", "!s", true},     // a slider is never blank
		{"n", "n != 0", true},      // truthy n is a non-zero number
		{"n > -2", "n >= -2", true},
		// Not judged: arithmetic, or comparing two inputs.
		{"n + 1 > 3", "n > 2", false},
		{"n > s", "n >= s", false},
	}
	for _, c := range cases {
		if got := neverUsable(c.vis, c.dis, kinds); got != c.want {
			t.Errorf("neverUsable(%q, %q) = %v, want %v", c.vis, c.dis, got, c.want)
		}
	}
	if !neverTrue("n > 5 && n < 3", kinds) || neverTrue("n > 3 && n < 5", kinds) {
		t.Error("contradictory and satisfiable ranges")
	}
}
