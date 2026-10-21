package httpapi

// theme_contrast.go derives the filled-button background from a bundle's
// palette so on_primary text on it always reaches WCAG AA (4.5:1).
//
// A bundle sets primary and on_primary as brand colors, and nothing forces the
// pair to contrast: fleet's own #7272ab under white is 4.46:1, and a yellow
// primary left with fleet's white on_primary is ~1.3:1. Filled buttons
// (Approve, Check result, Retry failed, Approve 2 of 3) therefore paint
// --color-primary-fill, not --color-primary: the primary mixed toward black or
// white by the smallest step that clears AA against on_primary. A pair that
// already passes is emitted unchanged. Because max(contrast with black,
// contrast with white) >= 4.58:1 for every color, one of the two directions
// always reaches 4.5:1, whatever the palette.
//
// The hover state mixes a further primaryFillHoverStep in the same direction,
// so hovering only ever raises the contrast (the old hover:opacity-90 washed
// the button into the card and dropped below AA).

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// primaryFillMinContrast is WCAG 1.4.3 AA for body-size text; button labels
	// are 0.75-0.8rem medium weight, so the large-text 3:1 never applies.
	primaryFillMinContrast = 4.5
	// primaryFillHoverStep is how much further (in percentage points of the
	// mix) the hover state moves away from the text color.
	primaryFillHoverStep = 10
)

// fleetPrimaryDefaults mirrors globals.css: fleet's --color-primary and
// --color-on-primary, identical in both themes. A bundle that sets only one of
// the pair is measured against fleet's value for the other.
// TestPrimaryFill_DefaultsMatchGlobalsCSS keeps the two in step.
var fleetPrimaryDefaults = struct{ primary, onPrimary string }{"#7272ab", "#ffffff"}

// rgba is a color with channels and alpha in [0,1].
type rgba struct{ r, g, b, a float64 }

// primaryFill is a derived fill: the primary's opaque color with percent of
// black (toward == "#000000") or white mixed in, plus its hover.
type primaryFill struct {
	fill, hover string  // #rrggbb
	toward      string  // "#000000" or "#ffffff"
	percent     int     // how much of the extreme is mixed in (0 = the primary itself)
	ratio       float64 // contrast of on_primary on fill, as the browser paints it
	hoverRatio  float64
}

// derivePrimaryFill returns the accessible fill for on-primary text over a
// primary, or ok=false when either color cannot be parsed (the stylesheet
// then keeps globals.css's default fill, which is derived from whatever
// --color-primary resolves to).
//
// A translucent primary is used opaque: a button fill must not depend on what
// it happens to sit on. A translucent on_primary is measured composited over
// each candidate fill, which is how the browser paints it.
func derivePrimaryFill(primary, onPrimary string) (primaryFill, bool) {
	p, ok := parseCSSColor(primary)
	if !ok {
		return primaryFill{}, false
	}
	text, ok := parseCSSColor(onPrimary)
	if !ok {
		return primaryFill{}, false
	}
	p.a = 1
	best := primaryFill{percent: -1}
	for _, toward := range []rgba{{0, 0, 0, 1}, {1, 1, 1, 1}} {
		for pct := 0; pct <= 100; pct++ {
			fill := quantize(mixSRGB(p, toward, pct))
			ratio := contrast(over(text, fill), fill)
			if ratio < primaryFillMinContrast {
				continue
			}
			hover := quantize(mixSRGB(p, toward, min(pct+primaryFillHoverStep, 100)))
			cand := primaryFill{
				fill:       hexOf(fill),
				hover:      hexOf(hover),
				toward:     hexOf(toward),
				percent:    pct,
				ratio:      ratio,
				hoverRatio: contrast(over(text, hover), hover),
			}
			// The smallest change wins. A primary that already passes (0% in
			// both directions) takes the direction that moves AWAY from the
			// text, so its hover raises the contrast instead of lowering it.
			if best.percent < 0 || cand.percent < best.percent ||
				(cand.percent == best.percent && cand.hoverRatio > best.hoverRatio) {
				best = cand
			}
			break
		}
	}
	// Unreachable for a parsed on_primary (see the file comment); kept so a
	// future change to the threshold cannot emit an unmeasured fill.
	if best.percent < 0 {
		return primaryFill{}, false
	}
	return best, true
}

// mixSRGB is CSS color-mix(in srgb, a (100-pct)%, b pct%) for opaque colors.
func mixSRGB(a, b rgba, pct int) rgba {
	t := float64(pct) / 100
	return rgba{
		a.r*(1-t) + b.r*t,
		a.g*(1-t) + b.g*t,
		a.b*(1-t) + b.b*t,
		1,
	}
}

// quantize rounds to the 8-bit color the emitted hex names.
func quantize(c rgba) rgba {
	q := func(v float64) float64 { return math.Round(clamp01(v)*255) / 255 }
	return rgba{q(c.r), q(c.g), q(c.b), c.a}
}

func hexOf(c rgba) string {
	b := func(v float64) int { return int(math.Round(clamp01(v) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", b(c.r), b(c.g), b(c.b))
}

// over composites top onto an opaque color.
func over(top, under rgba) rgba {
	a := top.a
	return rgba{
		top.r*a + under.r*(1-a),
		top.g*a + under.g*(1-a),
		top.b*a + under.b*(1-a),
		1,
	}
}

func luminance(c rgba) float64 {
	lin := func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

// contrast is the WCAG 2 contrast ratio of two opaque colors.
func contrast(a, b rgba) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func clamp01(v float64) float64 { return math.Min(math.Max(v, 0), 1) }

// parseCSSColor reads the color syntaxes colorValueRe admits: 3/4/6/8-digit
// hex, rgb()/rgba() and hsl()/hsla() in legacy comma or modern space syntax
// (with an optional "/ alpha"). Out-of-range components clamp, as CSS does.
// Anything else is not a color the browser would paint, so ok=false.
func parseCSSColor(s string) (rgba, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !colorValueRe.MatchString(s) {
		return rgba{}, false
	}
	if strings.HasPrefix(s, "#") {
		return parseHex(s[1:])
	}
	open := strings.IndexByte(s, '(')
	name, args := s[:open], strings.TrimSuffix(s[open+1:], ")")
	parts, ok := colorArgs(args)
	if !ok {
		return rgba{}, false
	}
	alpha := 1.0
	if len(parts) == 4 {
		if alpha, ok = cssNumber(parts[3], 1); !ok {
			return rgba{}, false
		}
	}
	switch name {
	case "rgb", "rgba":
		var ch [3]float64
		for i := range ch {
			if ch[i], ok = cssNumber(parts[i], 255); !ok {
				return rgba{}, false
			}
			ch[i] /= 255
		}
		return rgba{ch[0], ch[1], ch[2], alpha}, true
	case "hsl", "hsla":
		h, ok := cssHue(parts[0])
		if !ok {
			return rgba{}, false
		}
		sat, ok1 := cssNumber(parts[1], 100)
		light, ok2 := cssNumber(parts[2], 100)
		if !ok1 || !ok2 {
			return rgba{}, false
		}
		c := hslToRGB(h, sat/100, light/100)
		c.a = alpha
		return c, true
	}
	return rgba{}, false
}

func parseHex(x string) (rgba, bool) {
	if len(x) == 3 || len(x) == 4 {
		var b strings.Builder
		for _, ch := range x {
			b.WriteRune(ch)
			b.WriteRune(ch)
		}
		x = b.String()
	}
	if len(x) != 6 && len(x) != 8 {
		return rgba{}, false
	}
	n := func(i int) float64 {
		v, _ := strconv.ParseUint(x[i:i+2], 16, 8)
		return float64(v) / 255
	}
	c := rgba{n(0), n(2), n(4), 1}
	if len(x) == 8 {
		c.a = n(6)
	}
	return c, true
}

// colorArgs splits "a, b, c[, d]" or "a b c[ / d]" into 3 or 4 components.
func colorArgs(args string) ([]string, bool) {
	var parts []string
	if strings.Contains(args, ",") {
		if strings.Contains(args, "/") {
			return nil, false
		}
		for _, p := range strings.Split(args, ",") {
			parts = append(parts, strings.TrimSpace(p))
		}
	} else {
		main, alpha, hasAlpha := strings.Cut(args, "/")
		parts = strings.Fields(main)
		if hasAlpha {
			parts = append(parts, strings.TrimSpace(alpha))
		}
	}
	if len(parts) != 3 && len(parts) != 4 {
		return nil, false
	}
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t\n") {
			return nil, false
		}
	}
	return parts, true
}

// cssNumber reads a number, or a percentage of scale, clamped to [0, scale].
func cssNumber(s string, scale float64) (float64, bool) {
	pct := strings.HasSuffix(s, "%")
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if pct {
		v = v / 100 * scale
	}
	return math.Min(math.Max(v, 0), scale), true
}

// cssHue reads an hsl() hue in degrees (bare, deg, turn, rad or grad).
func cssHue(s string) (float64, bool) {
	unit := 1.0
	for _, u := range []struct {
		suffix string
		scale  float64
	}{{"deg", 1}, {"grad", 0.9}, {"rad", 180 / math.Pi}, {"turn", 360}} {
		if strings.HasSuffix(s, u.suffix) {
			s, unit = strings.TrimSuffix(s, u.suffix), u.scale
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return math.Mod(math.Mod(v*unit, 360)+360, 360), true
}

// hslToRGB is the CSS Color 4 hsl() conversion.
func hslToRGB(h, s, l float64) rgba {
	f := func(n float64) float64 {
		k := math.Mod(n+h/30, 12)
		a := s * math.Min(l, 1-l)
		return l - a*math.Max(-1, math.Min(math.Min(k-3, 9-k), 1))
	}
	return rgba{f(0), f(8), f(4), 1}
}
