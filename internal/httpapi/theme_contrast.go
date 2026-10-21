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
	"sort"
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

// primaryFill is a derived button pair: the fill (the primary's opaque color
// with percent of black, toward == "#000000", or white mixed in), its hover,
// and the label color painted on both.
type primaryFill struct {
	fill, hover string  // #rrggbb
	text        string  // #rrggbb: on_primary, opaque
	toward      string  // "#000000" or "#ffffff"
	percent     int     // how much of the extreme is mixed in (0 = the primary itself)
	ratio       float64 // contrast of text on fill
	hoverRatio  float64
}

// derivePrimaryFill returns the button pair for a primary and on_primary.
// It always returns an AA pair:
//
//   - The label is on_primary made opaque. A translucent label can sit
//     under 4.5:1 on every fill (white at 30% reaches 2.5:1 even on black),
//     so the button paints it solid. An opaque on_primary is unchanged.
//   - A translucent primary is used opaque: a button fill must not depend on
//     what it happens to sit on.
//   - A value fleet cannot read falls back to fleet's own for that color, so
//     the pair is still measured rather than left to an unmeasured mix.
//
// Every color reaches at least 4.58:1 against black or against white, so one
// of the two directions always passes for an opaque label.
func derivePrimaryFill(primary, onPrimary string) primaryFill {
	p, ok := parseCSSColor(primary)
	if !ok {
		p, _ = parseCSSColor(fleetPrimaryDefaults.primary)
	}
	text, ok := parseCSSColor(onPrimary)
	if !ok {
		text, _ = parseCSSColor(fleetPrimaryDefaults.onPrimary)
	}
	p.a, text.a = 1, 1
	text = quantize(text)
	best := primaryFill{percent: -1}
	for _, toward := range []rgba{{0, 0, 0, 1}, {1, 1, 1, 1}} {
		for pct := 0; pct <= 100; pct++ {
			fill := quantize(mixSRGB(p, toward, pct))
			ratio := contrast(text, fill)
			if ratio < primaryFillMinContrast {
				continue
			}
			hover := quantize(mixSRGB(p, toward, min(pct+primaryFillHoverStep, 100)))
			cand := primaryFill{
				fill:       hexOf(fill),
				hover:      hexOf(hover),
				text:       hexOf(text),
				toward:     hexOf(toward),
				percent:    pct,
				ratio:      ratio,
				hoverRatio: contrast(text, hover),
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
	return best
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
	legacy := strings.Contains(args, ",")
	alpha := 1.0
	if len(parts) == 4 {
		if alpha, ok = cssNumber(parts[3], 1); !ok {
			return rgba{}, false
		}
	}
	switch name {
	case "rgb", "rgba":
		// Legacy syntax: the channels are all numbers or all percentages.
		if legacy && (strings.HasSuffix(parts[0], "%") != strings.HasSuffix(parts[1], "%") ||
			strings.HasSuffix(parts[1], "%") != strings.HasSuffix(parts[2], "%")) {
			return rgba{}, false
		}
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
		// Legacy hsl() needs percentages for saturation and lightness.
		if legacy && (!strings.HasSuffix(parts[1], "%") || !strings.HasSuffix(parts[2], "%")) {
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
		// Modern syntax: exactly three space-separated components, the alpha
		// only after a slash ("rgb(1 2 3 4)" is not a color).
		main, alpha, hasAlpha := strings.Cut(args, "/")
		parts = strings.Fields(main)
		if len(parts) != 3 {
			return nil, false
		}
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

// The action gradient (the composer's Send circle, the empty-state prompt's
// Run button) runs from the primary to the secondary. Its label sits on every
// point of it, so both stops are measured, and so is the blend between them:
// with dark text a straight sRGB blend can dip darker mid-way than either end
// (luminance is convex along the blend), so an AA pair of ends is not enough:
// blendMinContrast measures every color the blend paints.
// The gradient starts at the measured primary fill and ends at
// --color-secondary-fill: the secondary mixed toward the fill's extreme by the
// smallest step at which every point of the blend clears AA under the label.
// Its hover runs from the primary hover to a secondary hover at least
// primaryFillHoverStep further, measured the same way.

// fleetSecondaryDefault mirrors globals.css's --color-secondary (both themes).
// TestSecondaryFill_DefaultsMatchGlobalsCSS keeps the two in step.
const fleetSecondaryDefault = "#586f7c"

// secondaryFill is the measured end of the action gradient and its hover.
type secondaryFill struct {
	fill, hover        string // #rrggbb
	percent, hoverPct  int    // how much of the extreme is mixed in
	minRatio, hoverMin float64
}

// deriveSecondaryFill returns the action gradient's end stops for a derived
// primary pair and a secondary. Its blends with the primary fill (and the
// hover's with the primary hover) clear AA at every point: at 100% the end is
// the extreme the fill was mixed toward, and a blend from an AA fill toward
// that extreme only moves away from the label. Should that ever not hold, the
// end is the start stop itself (a flat fill, AA by construction).
func deriveSecondaryFill(pf primaryFill, secondary string) secondaryFill {
	s, ok := parseCSSColor(secondary)
	if !ok {
		s, _ = parseCSSColor(fleetSecondaryDefault)
	}
	s.a = 1
	text, _ := parseCSSColor(pf.text)
	fill, _ := parseCSSColor(pf.fill)
	hover, _ := parseCSSColor(pf.hover)
	toward, _ := parseCSSColor(pf.toward)
	other := rgba{1 - toward.r, 1 - toward.g, 1 - toward.b, 1}

	// end is the smallest mix of s (from `from`% up) whose blend from start
	// clears AA everywhere.
	end := func(start rgba, from int) (rgba, int, float64) {
		for _, dir := range []rgba{toward, other} {
			for pct := from; pct <= 100; pct++ {
				cand := quantize(mixSRGB(s, dir, pct))
				if r := blendMinContrast(text, start, cand); r >= primaryFillMinContrast {
					return cand, pct, r
				}
			}
		}
		return start, 0, blendMinContrast(text, start, start)
	}
	var out secondaryFill
	c, pct, r := end(fill, 0)
	out.fill, out.percent, out.minRatio = hexOf(c), pct, r
	c, pct, r = end(hover, min(out.percent+primaryFillHoverStep, 100))
	out.hover, out.hoverPct, out.hoverMin = hexOf(c), pct, r
	return out
}

// blendMinContrast is the lowest contrast of text on any color a CSS
// linear-gradient from a to b paints (opaque stops blend in sRGB, and each
// point is painted as its 8-bit color). Sampling fixed points is not enough:
// where the pixels fall depends on the element's size, and the worst color
// can sit between any two samples. So it measures every color the blend can
// paint. A channel's 8-bit value changes only where the blend crosses a
// half-step (k+0.5)/255, so between two consecutive crossings of any channel
// the painted color is constant: one point inside each such interval, plus
// the two ends, is the whole set (at most 766 colors).
func blendMinContrast(text, a, b rgba) float64 {
	from := [3]float64{a.r * 255, a.g * 255, a.b * 255}
	to := [3]float64{b.r * 255, b.g * 255, b.b * 255}
	cuts := []float64{0, 1}
	for i := range 3 {
		lo, hi := math.Min(from[i], to[i]), math.Max(from[i], to[i])
		for k := math.Floor(lo) + 0.5; k < hi; k++ {
			if k > lo {
				cuts = append(cuts, (k-from[i])/(to[i]-from[i]))
			}
		}
	}
	sort.Float64s(cuts)
	at := func(t float64) rgba {
		return quantize(rgba{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t, 1})
	}
	lowest := math.Min(contrast(text, at(0)), contrast(text, at(1)))
	for j := 0; j+1 < len(cuts); j++ {
		if cuts[j+1] > cuts[j] {
			lowest = math.Min(lowest, contrast(text, at((cuts[j]+cuts[j+1])/2)))
		}
	}
	return lowest
}
