package httpapi

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// measuredRatio re-measures a derived pair from the hex it emits,
// independent of the values derivePrimaryFill reports about itself.
func measuredRatio(t *testing.T, textHex, fillHex string) float64 {
	t.Helper()
	text, ok := parseCSSColor(textHex)
	if !ok || text.a != 1 {
		t.Fatalf("label %q is not an opaque color", textHex)
	}
	fill, ok := parseCSSColor(fillHex)
	if !ok || fill.a != 1 {
		t.Fatalf("fill %q is not an opaque color", fillHex)
	}
	return contrast(text, fill)
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
			f := derivePrimaryFill(c.primary, c.onPrimary)
			if f.fill != c.fill || f.hover != c.hover || f.percent != c.percent {
				t.Errorf("got fill %s hover %s at %d%% toward %s, want fill %s hover %s at %d%%",
					f.fill, f.hover, f.percent, f.toward, c.fill, c.hover, c.percent)
			}
			if r := measuredRatio(t, f.text, f.fill); r < primaryFillMinContrast {
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
		f := derivePrimaryFill(p[0], p[1])
		if r := measuredRatio(t, f.text, f.fill); r < primaryFillMinContrast {
			t.Fatalf("%s on %s → %s on fill %s: %.3f:1 < 4.5:1", p[1], p[0], f.text, f.fill, r)
		}
		if h := measuredRatio(t, f.text, f.hover); h < measuredRatio(t, f.text, f.fill)-1e-9 {
			t.Fatalf("%s on %s → hover %s (%.3f:1) lowers the fill's contrast", p[1], p[0], f.hover, h)
		}
	}
}

// A value fleet cannot read falls back to fleet's own for that color, and
// a translucent label is painted solid, so the pair is always measured.
func TestDerivePrimaryFill_FallsBackAndPaintsTheLabelSolid(t *testing.T) {
	for _, c := range []struct{ primary, onPrimary, fill, text string }{
		{"rgb(foo, bar, baz)", "#ffffff", "#7171a9", "#ffffff"},
		{"rgb(1 2 3 4)", "#ffffff", "#7171a9", "#ffffff"},
		{"#7272ab", "rgb(1, 2)", "#7171a9", "#ffffff"},
		{"hsl(240, 25, 56)", "#ffffff", "#7171a9", "#ffffff"},
		{"rgb(100%, 0, 0)", "#ffffff", "#7171a9", "#ffffff"},
		{"", "", "#7171a9", "#ffffff"},
		// White at 30% is 2.46:1 even on black; painted solid it passes.
		{"#7272ab", "rgba(255, 255, 255, 0.3)", "#7171a9", "#ffffff"},
	} {
		f := derivePrimaryFill(c.primary, c.onPrimary)
		if f.fill != c.fill || f.text != c.text {
			t.Errorf("derivePrimaryFill(%q, %q) = %s on %s, want %s on %s", c.primary, c.onPrimary, f.text, f.fill, c.text, c.fill)
		}
		if r := measuredRatio(t, f.text, f.fill); r < primaryFillMinContrast {
			t.Errorf("%q / %q: %.3f:1", c.primary, c.onPrimary, r)
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
	f := derivePrimaryFill(fleetPrimaryDefaults.primary, fleetPrimaryDefaults.onPrimary)
	if f.toward != "#000000" {
		t.Fatalf("fleet default fill: %+v", f)
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
		if !strings.Contains(css, "--color-primary-fill:#877602;--color-primary-fill-hover:#6e6001;--color-on-primary-fill:#ffffff;") {
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
		Text      string `json:"text"`
		// The action gradient's end: the secondary, and the measured stops
		// deriveSecondaryFill pairs with fill and hover.
		Secondary      string `json:"secondary"`
		SecondaryFill  string `json:"secondary_fill"`
		SecondaryHover string `json:"secondary_hover"`
	}
	traits := []struct{ name, primary, onPrimary string }{
		{"fleet's own palette", "#7272ab", "#ffffff"},
		{"a light (yellow) primary with dark text", "#ffdf03", "#0c0a09"},
		{"a light (yellow) primary left with fleet's white text", "#ffdf03", "#ffffff"},
		{"a mid-gray primary under white", "#8a8a8a", "#ffffff"},
		{"a dark primary with dark text", "#1d2a5a", "#000000"},
		{"a translucent primary", "rgba(114, 114, 171, 0.5)", "#ffffff"},
		{"translucent text", "#7272ab", "rgba(255, 255, 255, 0.85)"},
		{"faint text no fill can carry", "#7272ab", "rgba(255, 255, 255, 0.3)"},
		{"a saturated magenta primary under white", "#e5007a", "#ffffff"},
		// These run to their own secondary (below); the rest to fleet's.
		{"a pale secondary under white", "#7272ab", "#ffffff"},
		{"a deep secondary under dark text", "#ffdf03", "#0c0a09"},
		{"a blend that dips between AA ends", "#ee0000", "#000000"},
		{"a translucent secondary", "#7272ab", "#ffffff"},
	}
	// A pale secondary under white, a deep one under dark text, and a pair
	// whose ends clear AA under dark text while the blend between them dips
	// under it.
	secondaries := map[string]string{
		"a pale secondary under white":      "#c8e6f5",
		"a deep secondary under dark text":  "#123d2a",
		"a blend that dips between AA ends": "#008844",
		"a translucent secondary":           "rgba(88, 111, 124, 0.4)",
	}
	got := make([]entry, 0, len(traits))
	for _, tr := range traits {
		f := derivePrimaryFill(tr.primary, tr.onPrimary)
		secondary := fleetSecondaryDefault
		if v, ok := secondaries[tr.name]; ok {
			secondary = v
		}
		g := deriveSecondaryFill(f, secondary)
		got = append(got, entry{tr.name, tr.primary, tr.onPrimary, f.fill, f.hover, f.text, secondary, g.fill, g.hover})
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

// The action gradient's guarantee: for ANY palette, every point of the blend
// from the primary fill to the secondary fill (and from the primary hover to
// the secondary hover) clears AA under the label, re-measured from the hex the
// derivation emits.
func TestDeriveSecondaryFill_AnyPaletteBlendReachesAA(t *testing.T) {
	rng := rand.New(rand.NewPCG(1830, 7))
	randomHex := func() string { return fmt.Sprintf("#%06x", rng.IntN(1<<24)) }
	for range 1000 {
		primary, onPrimary, secondary := randomHex(), randomHex(), randomHex()
		f := derivePrimaryFill(primary, onPrimary)
		g := deriveSecondaryFill(f, secondary)
		text, _ := parseCSSColor(f.text)
		for _, seg := range [][2]string{{f.fill, g.fill}, {f.hover, g.hover}} {
			a, _ := parseCSSColor(seg[0])
			b, _ := parseCSSColor(seg[1])
			if r := blendMinContrast(text, a, b); r < primaryFillMinContrast {
				t.Fatalf("%s on %s → %s (secondary %s): %.3f:1 somewhere on the blend", f.text, seg[0], seg[1], secondary, r)
			}
		}
	}
}

// A pair whose ends both clear AA under dark text can dip under it mid-way:
// red #ee0000 and green #008844 under black are 4.6:1 each, and their sRGB
// blend falls to about 2.6:1 mid-way. The end is pushed until the blend holds.
func TestDeriveSecondaryFill_MeasuresTheBlendNotJustTheEnds(t *testing.T) {
	f := derivePrimaryFill("#ee0000", "#000000")
	text, _ := parseCSSColor(f.text)
	fill, _ := parseCSSColor(f.fill)
	green, _ := parseCSSColor("#008844")
	if contrast(text, green) < primaryFillMinContrast || contrast(text, fill) < primaryFillMinContrast {
		t.Fatalf("the ends should pass on their own: %s and #008844 under %s", f.fill, f.text)
	}
	if r := blendMinContrast(text, fill, green); r >= primaryFillMinContrast {
		t.Fatalf("the raw blend should dip under AA, got %.3f:1", r)
	}
	g := deriveSecondaryFill(f, "#008844")
	if g.fill == "#008844" || g.minRatio < primaryFillMinContrast {
		t.Errorf("secondary fill %s (%.3f:1): the blend was not measured", g.fill, g.minRatio)
	}
}

// fleet's own secondary already clears AA along the gradient, so globals.css
// declares the fill as the secondary itself and the hover as a mix toward
// black. Both must be what deriveSecondaryFill computes for fleet's palette.
func TestSecondaryFill_DefaultsMatchGlobalsCSS(t *testing.T) {
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
	for _, v := range decl("--color-secondary") {
		if !strings.EqualFold(v, fleetSecondaryDefault) {
			t.Errorf("globals.css --color-secondary: %s, fleetSecondaryDefault is %s", v, fleetSecondaryDefault)
		}
	}
	g := deriveSecondaryFill(derivePrimaryFill(fleetPrimaryDefaults.primary, fleetPrimaryDefaults.onPrimary), fleetSecondaryDefault)
	if g.percent != 0 {
		t.Fatalf("fleet's secondary should need no mix, got %+v", g)
	}
	if vals := decl("--color-secondary-fill"); len(vals) != 1 || vals[0] != "var(--color-secondary)" {
		t.Errorf("globals.css --color-secondary-fill: %q, want var(--color-secondary) once", vals)
	}
	want := fmt.Sprintf("color-mix(in srgb, var(--color-secondary) %d%%, #000)", 100-g.hoverPct)
	if vals := decl("--color-secondary-fill-hover"); len(vals) != 1 || vals[0] != want {
		t.Errorf("globals.css --color-secondary-fill-hover: %q, want %q once", vals, want)
	}
}

func TestRenderThemeCSS_EmitsSecondaryFill(t *testing.T) {
	t.Run("secondary only: measured on fleet's primary pair, which stays the default", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Dark: map[string]string{"secondary": "#c8e6f5"}})
		if strings.Contains(css, "primary-fill") || !strings.Contains(css, "--color-secondary-fill:#") {
			t.Errorf("got %q", css)
		}
	})
	t.Run("primary only: the gradient end is re-measured against the new fill", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Light: map[string]string{"primary": "#ffdf03"}})
		g := deriveSecondaryFill(derivePrimaryFill("#ffdf03", fleetPrimaryDefaults.onPrimary), fleetSecondaryDefault)
		if !strings.Contains(css, fmt.Sprintf("--color-secondary-fill:%s;--color-secondary-fill-hover:%s;", g.fill, g.hover)) {
			t.Errorf("got %q", css)
		}
	})
	t.Run("none of the three: globals.css default stands", func(t *testing.T) {
		css := renderThemeCSS(clientconfig.BrandColors{Dark: map[string]string{"accent": "#9da7ef"}})
		if strings.Contains(css, "secondary-fill") {
			t.Errorf("got %q", css)
		}
	})
}

// blendScan is a brute-force scan of a blend: the lowest contrast of text on
// the 8-bit color painted at each of steps+1 evenly spaced points.
func blendScan(text, a, b rgba, steps int) (float64, rgba) {
	lowest, worst := math.Inf(1), a
	for i := 0; i <= steps; i++ {
		u := float64(i) / float64(steps)
		p := quantize(rgba{a.r + (b.r-a.r)*u, a.g + (b.g-a.g)*u, a.b + (b.b-a.b)*u, 1})
		if r := contrast(text, p); r < lowest {
			lowest, worst = r, p
		}
	}
	return lowest, worst
}

// blendMinContrast must find the worst color the blend paints wherever the
// pixels fall, not the worst of a few samples: it is never above a
// brute-force scan 10,000 steps fine over random blends.
func TestBlendMinContrast_FindsEveryPaintedColor(t *testing.T) {
	rng := rand.New(rand.NewPCG(1830, 11))
	randomColor := func() rgba {
		return rgba{float64(rng.IntN(256)) / 255, float64(rng.IntN(256)) / 255, float64(rng.IntN(256)) / 255, 1}
	}
	for range 200 {
		text, a, b := randomColor(), randomColor(), randomColor()
		brute, _ := blendScan(text, a, b, 10000)
		if got := blendMinContrast(text, a, b); got > brute+1e-12 {
			t.Fatalf("%s → %s under %s: blendMinContrast %.6f, a painted color reaches %.6f", hexOf(a), hexOf(b), hexOf(text), got, brute)
		}
	}
}

// Codex review (#1830): with 65 fixed samples this palette's gradient
// measured 4.5000:1 while a painted color between two samples was 4.477:1.
func TestDeriveSecondaryFill_NoPaintedColorBetweenSamplesFallsUnderAA(t *testing.T) {
	f := derivePrimaryFill("#920a39", "#360a91")
	g := deriveSecondaryFill(f, "#08c1e4")
	text, _ := parseCSSColor(f.text)
	for _, seg := range [][2]string{{f.fill, g.fill}, {f.hover, g.hover}} {
		a, _ := parseCSSColor(seg[0])
		b, _ := parseCSSColor(seg[1])
		if r, p := blendScan(text, a, b, 50000); r < primaryFillMinContrast {
			t.Fatalf("%s → %s paints %s at %.4f:1 under %s", seg[0], seg[1], hexOf(p), r, f.text)
		}
	}
}
