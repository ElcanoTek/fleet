package httpapi

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// measuredRatio re-measures a derived fill from its emitted hex, independent
// of the values derivePrimaryFill reports about itself.
func measuredRatio(t *testing.T, onPrimary, fillHex string) float64 {
	t.Helper()
	text, ok := parseCSSColor(onPrimary)
	if !ok {
		t.Fatalf("on_primary %q does not parse", onPrimary)
	}
	fill, ok := parseCSSColor(fillHex)
	if !ok || fill.a != 1 {
		t.Fatalf("fill %q is not an opaque color", fillHex)
	}
	return contrast(over(text, fill), fill)
}

func TestDerivePrimaryFill_Cases(t *testing.T) {
	cases := []struct {
		name, primary, onPrimary string
		fill, hover              string
		percent                  int
	}{
		// White on #7272ab is 4.46:1: one point of black clears AA.
		{"fleet default", "#7272ab", "#ffffff", "#7171a9", "#656598", 1},
		// Already 4.5:1 or better: the fill IS the primary, and the hover moves
		// away from the dark text (toward white).
		{"yellow primary, dark text", "#ffdf03", "#0c0a09", "#ffdf03", "#ffe21c", 0},
		{"deep primary, white text", "#2b2b6b", "#ffffff", "#2b2b6b", "#272760", 0},
		// A yellow primary left with fleet's white text is ~1.3:1; the fill
		// darkens until white reads on it.
		{"yellow primary, white text", "#ffdf03", "#ffffff", "#877602", "#6e6001", 47},
		// Dark text on a dark primary: lightened instead.
		{"navy primary, black text", "#1d2a5a", "#000000", "#6c7594", "#838aa4", 35},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, ok := derivePrimaryFill(c.primary, c.onPrimary)
			if !ok {
				t.Fatalf("derivePrimaryFill(%q, %q) not ok", c.primary, c.onPrimary)
			}
			if f.fill != c.fill || f.hover != c.hover || f.percent != c.percent {
				t.Errorf("got fill %s hover %s at %d%% toward %s, want fill %s hover %s at %d%%",
					f.fill, f.hover, f.percent, f.toward, c.fill, c.hover, c.percent)
			}
			if r := measuredRatio(t, c.onPrimary, f.fill); r < primaryFillMinContrast {
				t.Errorf("fill %s: %.3f:1 < 4.5:1", f.fill, r)
			}
		})
	}
}

// The guarantee: for ANY palette the fill reaches AA, measured from the hex
// it emits (8-bit, as the browser paints it), and the hover never lowers it.
func TestDerivePrimaryFill_AnyPaletteReachesAA(t *testing.T) {
	rng := rand.New(rand.NewPCG(1815, 4))
	randomHex := func() string { return fmt.Sprintf("#%06x", rng.IntN(1<<24)) }
	pairs := make([][2]string, 0, 20008)
	pairs = append(pairs, [][2]string{
		// Every syntax colorValueRe admits, translucent ones included.
		{"rgb(114, 114, 171)", "rgba(255, 255, 255, 0.92)"},
		{"rgb(114 114 171 / 50%)", "#fff"},
		{"hsl(240, 25%, 56%)", "hsl(0 0% 100%)"},
		{"hsla(54deg 100% 51% / 0.4)", "#ffffffcc"},
		{"hsl(0.15turn, 90%, 50%)", "#0c0a09"},
		{"#7272ab80", "#f0f"},
		// The colors whose best contrast is closest to 4.5:1 either way.
		{"#777777", "#767676"},
		{"#757575", "#777777"},
	}...)
	for range 20000 {
		pairs = append(pairs, [2]string{randomHex(), randomHex()})
	}
	for _, p := range pairs {
		f, ok := derivePrimaryFill(p[0], p[1])
		if !ok {
			t.Fatalf("derivePrimaryFill(%q, %q) not ok", p[0], p[1])
		}
		if r := measuredRatio(t, p[1], f.fill); r < primaryFillMinContrast {
			t.Fatalf("%s on %s → fill %s: %.3f:1 < 4.5:1", p[1], p[0], f.fill, r)
		}
		if h := measuredRatio(t, p[1], f.hover); h < measuredRatio(t, p[1], f.fill)-1e-9 {
			t.Fatalf("%s on %s → hover %s (%.3f:1) lowers the fill's contrast", p[1], p[0], f.hover, h)
		}
	}
}

func TestDerivePrimaryFill_RejectsUnparseable(t *testing.T) {
	for _, p := range [][2]string{
		{"rgb(foo, bar, baz)", "#ffffff"},
		{"#7272ab", "rgb(1, 2)"},
		{"hsl(1, 2%, 3%, 4, 5)", "#ffffff"},
		{"rgb(1, 2, 3 / 0.5)", "#ffffff"},
		{"red", "#ffffff"},
		{"", "#ffffff"},
	} {
		if f, ok := derivePrimaryFill(p[0], p[1]); ok {
			t.Errorf("derivePrimaryFill(%q, %q) = %+v, want not ok", p[0], p[1], f)
		}
	}
}

func TestParseCSSColor(t *testing.T) {
	for in, want := range map[string]string{
		"#abc":                        "#aabbcc",
		"#AABBCC":                     "#aabbcc",
		"rgb(114, 114, 171)":          "#7272ab",
		"rgba(100%, 0%, 50%, 0.5)":    "#ff0080",
		"rgb(300 0 171)":              "#ff00ab",
		"hsl(240, 100%, 50%)":         "#0000ff",
		"hsl(120deg 100% 25% / 1)":    "#008000",
		"hsla(0.5turn, 100%, 50%)":    "#00ffff",
		"hsl(3.14159265rad 100% 50%)": "#00ffff",
	} {
		c, ok := parseCSSColor(in)
		if !ok || hexOf(c) != want {
			t.Errorf("parseCSSColor(%q) = %s, %v; want %s", in, hexOf(c), ok, want)
		}
	}
}

// globals.css carries fleet's own fill as color-mix() of the primary (so an
// unparseable bundle primary still gets a fill in its own hue). Its mix must
// be exactly what derivePrimaryFill computes for fleet's pair, in both themes.
func TestPrimaryFill_DefaultsMatchGlobalsCSS(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "app", "globals.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(raw), "")
	decl := func(name string) []string {
		ms := regexp.MustCompile(`(?m)^\s*`+regexp.QuoteMeta(name)+`:\s*([^;]+);`).FindAllStringSubmatch(css, -1)
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			out = append(out, strings.TrimSpace(m[1]))
		}
		return out
	}
	for name, want := range map[string]string{
		"--color-primary":    fleetPrimaryDefaults.primary,
		"--color-on-primary": fleetPrimaryDefaults.onPrimary,
	} {
		vals := decl(name)
		if len(vals) == 0 {
			t.Fatalf("globals.css declares no %s", name)
		}
		for _, v := range vals {
			if !strings.EqualFold(v, want) {
				t.Errorf("globals.css %s: %s, fleetPrimaryDefaults has %s", name, v, want)
			}
		}
	}
	f, ok := derivePrimaryFill(fleetPrimaryDefaults.primary, fleetPrimaryDefaults.onPrimary)
	if !ok || f.toward != "#000000" {
		t.Fatalf("fleet default fill: %+v, %v", f, ok)
	}
	mix := regexp.MustCompile(`^color-mix\(in srgb, var\(--color-primary\) (\d+)%, #000\)$`)
	for name, pct := range map[string]int{
		"--color-primary-fill":       f.percent,
		"--color-primary-fill-hover": f.percent + primaryFillHoverStep,
	} {
		vals := decl(name)
		if len(vals) != 1 {
			t.Fatalf("globals.css must declare %s once (it follows the theme's primary): %q", name, vals)
		}
		m := mix.FindStringSubmatch(vals[0])
		if m == nil {
			t.Fatalf("globals.css %s: %q is not color-mix(in srgb, var(--color-primary) N%%, #000)", name, vals[0])
		}
		if n, _ := strconv.Atoi(m[1]); n != 100-pct {
			t.Errorf("globals.css %s mixes %d%% primary; derivePrimaryFill needs %d%%", name, n, 100-pct)
		}
	}
}

func TestRenderThemeCSS_EmitsPrimaryFill(t *testing.T) {
	t.Run("primary only: measured against fleet's on_primary", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Light: map[string]string{"primary": "#ffdf03"}})
		if !strings.Contains(css, "--color-primary-fill:#877602;--color-primary-fill-hover:#6e6001;") {
			t.Errorf("no measured fill: %q", css)
		}
	})
	t.Run("on_primary only: measured against fleet's primary", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Dark: map[string]string{"on_primary": "#ffffff"}})
		if !strings.Contains(css, "--color-primary-fill:#7171a9;") {
			t.Errorf("no measured fill: %q", css)
		}
	})
	t.Run("neither set: globals.css default stands", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Dark: map[string]string{"accent": "#9da7ef"}})
		if strings.Contains(css, "primary-fill") {
			t.Errorf("emitted a fill without a primary pair: %q", css)
		}
	})
	t.Run("a pinned fill is not a theme token", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Dark: map[string]string{"primary_fill": "#7272ab"}})
		if strings.Contains(css, "primary-fill") {
			t.Errorf("a bundle pinned the fill: %q", css)
		}
	})
	t.Run("invalid primary: dropped, fill measured on fleet's", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Dark: map[string]string{"primary": "red; }", "on_primary": "#ffffff"}})
		if strings.Contains(css, "--color-primary:") || !strings.Contains(css, "--color-primary-fill:#7171a9;") {
			t.Errorf("got %q", css)
		}
	})
}

// testdata/contracts/theme-primary-fill.json is what /theme.css emits for a
// set of palette traits. The web contrast test re-measures each with its own
// evaluator (approvalCardContrast.test.ts), so the two implementations check
// each other. Regenerate after changing the derivation:
//
//	go test ./internal/httpapi -run TestPrimaryFillContract -update
func TestPrimaryFillContract(t *testing.T) {
	type entry struct {
		Name      string `json:"name"`
		Primary   string `json:"primary"`
		OnPrimary string `json:"on_primary"`
		Fill      string `json:"fill"`
		Hover     string `json:"hover"`
	}
	traits := []struct{ name, primary, onPrimary string }{
		{"fleet's own palette", "#7272ab", "#ffffff"},
		{"a light (yellow) primary with dark text", "#ffdf03", "#0c0a09"},
		{"a light (yellow) primary left with fleet's white text", "#ffdf03", "#ffffff"},
		{"a mid-gray primary under white", "#8a8a8a", "#ffffff"},
		{"a dark primary with dark text", "#1d2a5a", "#000000"},
		{"a translucent primary", "rgba(114, 114, 171, 0.5)", "#ffffff"},
		{"translucent text", "#7272ab", "rgba(255, 255, 255, 0.85)"},
		{"a saturated magenta primary under white", "#e5007a", "#ffffff"},
	}
	got := make([]entry, 0, len(traits))
	for _, tr := range traits {
		f, ok := derivePrimaryFill(tr.primary, tr.onPrimary)
		if !ok {
			t.Fatalf("%s: not ok", tr.name)
		}
		got = append(got, entry{tr.name, tr.primary, tr.onPrimary, f.fill, f.hover})
	}
	body, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	path := filepath.Join("..", "..", "testdata", "contracts", "theme-primary-fill.json")
	if *updateContract {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with -update)", err)
	}
	if string(want) != string(body) {
		t.Errorf("%s is stale; regenerate with -update.\nwant:\n%s\ngot:\n%s", path, want, body)
	}
}
