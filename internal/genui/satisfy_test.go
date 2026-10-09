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
	bools := map[string]inputKind{"gate": {kind: kindBool}}
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
	kinds := map[string]inputKind{"n": {kind: kindNumber}, "s": {kind: kindSlider}}
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

func TestSliderDomain(t *testing.T) {
	kinds := map[string]inputKind{"s": {kind: kindSlider, domain: &numDomain{min: 0, max: 10, hasMin: true, hasMax: true, step: 1}}}
	cases := []struct {
		vis, dis string
		want     bool
	}{
		{"s > 10", "", true},         // the slider never goes past 10
		{"s < 0", "", true},          // nor below 0
		{"s > 9.5", "s >= 10", true}, // the only position above 9.5 is 10
		{"s > 9", "", false},         // 10
		{"s == 2.5", "", true},       // off the step grid
		{"s >= 3 && s <= 4", "", false},
	}
	for _, c := range cases {
		if got := neverUsable(c.vis, c.dis, kinds); got != c.want {
			t.Errorf("neverUsable(%q, %q) = %v, want %v", c.vis, c.dis, got, c.want)
		}
	}
}

func TestSliderDomainDecimals(t *testing.T) {
	kinds := map[string]inputKind{"s": {kind: kindSlider, domain: &numDomain{min: 0, max: 1, hasMin: true, hasMax: true, step: 0.1}}}
	if neverUsable("s == 0.3", "", kinds) {
		t.Error("0.3 is a position of a 0..1 slider stepping by 0.1")
	}
}

func TestNumberDomainForValidatingActions(t *testing.T) {
	in := func(d *numDomain) map[string]inputKind {
		return map[string]inputKind{"n": {kind: kindNumber, domain: d}}
	}
	cases := []struct {
		name     string
		d        *numDomain
		vis, dis string
		want     bool
	}{
		{"past max", &numDomain{min: 0, max: 10, hasMin: true, hasMax: true}, "n > 10", "", true},
		{"below min, blank allowed", &numDomain{min: 0, hasMin: true, blank: true}, "n < 0", "", true}, // blank orders as 0
		{"below min, required", &numDomain{min: 0, hasMin: true}, "n < 0", "", true},
		{"within range", &numDomain{min: 0, max: 10, hasMin: true, hasMax: true}, "n > 9", "", false},
		{"only max", &numDomain{max: 10, hasMax: true}, "n > 10", "", true},
		{"step grid", &numDomain{min: 0, max: 10, hasMin: true, hasMax: true, step: 2}, "n == 3", "", true},
		{"no domain", nil, "n > 10", "", false},
		// A grid with one bound, none, or too many positions to scan.
		{"step grid, min only", &numDomain{min: 0, hasMin: true, step: 2}, "n == 1", "", true},
		{"step grid, min only, on grid", &numDomain{min: 0, hasMin: true, step: 2}, "n == 4", "", false},
		{"step grid, min only, region", &numDomain{min: 0, hasMin: true, step: 2}, "n > 1 && n < 3", "", false},
		{"step grid, min only, empty region", &numDomain{min: 1, hasMin: true, step: 2}, "n > 1 && n < 3", "", true},
		{"step only", &numDomain{step: 0.5}, "n > 0.1 && n < 0.4", "", true},
		{"step only, decimals", &numDomain{step: 0.1}, "n == 0.3", "", false},
		{"step grid, too fine to scan", &numDomain{min: 0, max: 1e9, hasMin: true, hasMax: true, step: 2}, "n == 7", "", true},
		{"step finer than the rounding", &numDomain{min: 0, hasMin: true, step: 0.0000000007}, "n == 0.000000001", "", true},
		{"step finer than the rounding, on grid", &numDomain{min: 0, hasMin: true, step: 0.0000000007}, "n > 0.000000001 && n < 0.0000000015", "", false},
		{"step grid, too fine to scan, beyond literals", &numDomain{min: 0, max: 1e9, hasMin: true, hasMax: true, step: 2}, "n > 7", "n == 8", false},
	}
	for _, c := range cases {
		if got := neverUsable(c.vis, c.dis, in(c.d)); got != c.want {
			t.Errorf("%s: neverUsable(%q, %q) = %v, want %v", c.name, c.vis, c.dis, got, c.want)
		}
	}
}
