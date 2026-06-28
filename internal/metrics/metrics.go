// Package metrics is a tiny, dependency-free Prometheus text-format exporter for
// fleet (#176). It deliberately avoids the prometheus/client_golang dependency —
// the surface fleet needs (a handful of labeled counters, two pull-at-scrape
// gauges, one latency histogram) is small enough to hand-roll, matching the
// no-new-dependency ethos of the rest of the codebase.
//
// All exported Record*/Observe helpers are safe for concurrent use. Gauges that
// reflect live state (active agents, sandbox pool depth) are registered as
// callbacks and evaluated at scrape time via Render.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var reg = &registry{
	counters:   map[string]*counterVec{},
	histograms: map[string]*histogramVec{},
	gauges:     map[string]*gaugeVec{},
}

type registry struct {
	mu           sync.Mutex
	counters     map[string]*counterVec
	histograms   map[string]*histogramVec
	gauges       map[string]*gaugeVec // imperatively-set gauges (last value wins)
	gaugeFuncs   []gaugeFunc
	counterFuncs []gaugeFunc // pull-at-scrape counters (cumulative values read live)
}

// ── counters ────────────────────────────────────────────────────────────────

type counterVec struct {
	help   string
	labels []string
	values map[string]float64 // serialized-labelset → value
}

// incCounter adds `by` to the counter `name` for the given label values (which
// must align with labelNames). Registers the family on first use.
func incCounter(name, help string, labelNames []string, labelVals []string, by float64) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	c := reg.counters[name]
	if c == nil {
		c = &counterVec{help: help, labels: labelNames, values: map[string]float64{}}
		reg.counters[name] = c
	}
	c.values[seriesKey(labelNames, labelVals)] += by
}

// ── gauges (imperatively set) ────────────────────────────────────────────────

// gaugeVec is a settable gauge family: each Set overwrites the value for its
// labelset (last write wins), unlike a counter which accumulates. Used for
// point-in-time measurements pushed at the end of an event (e.g. a task run's
// peak sandbox CPU/memory, #263) rather than pulled at scrape.
type gaugeVec struct {
	help   string
	labels []string
	values map[string]float64 // serialized-labelset → last value
}

// setGauge overwrites the gauge `name` for the given label values. Registers the
// family on first use.
func setGauge(name, help string, labelNames, labelVals []string, v float64) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	g := reg.gauges[name]
	if g == nil {
		g = &gaugeVec{help: help, labels: labelNames, values: map[string]float64{}}
		reg.gauges[name] = g
	}
	g.values[seriesKey(labelNames, labelVals)] = v
}

// ── histograms ───────────────────────────────────────────────────────────────

var defaultBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

type histogramVec struct {
	help    string
	labels  []string
	buckets []float64
	series  map[string]*histSeries
}

type histSeries struct {
	counts []uint64 // per bucket (cumulative computed at render)
	sum    float64
	count  uint64
}

func observeHistogram(name, help string, labelNames, labelVals []string, v float64) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	h := reg.histograms[name]
	if h == nil {
		h = &histogramVec{help: help, labels: labelNames, buckets: defaultBuckets, series: map[string]*histSeries{}}
		reg.histograms[name] = h
	}
	key := seriesKey(labelNames, labelVals)
	s := h.series[key]
	if s == nil {
		s = &histSeries{counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	s.sum += v
	s.count++
	for i, b := range h.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
}

// ── gauges (pull at scrape) ──────────────────────────────────────────────────

type gaugeFunc struct {
	name   string
	help   string
	labels []string
	fn     func() []GaugeSample
}

// GaugeSample is one labeled gauge value produced by a registered callback.
type GaugeSample struct {
	Labels []string // values aligned with the gauge's label names
	Value  float64
}

// RegisterGauge wires a pull-at-scrape gauge: fn is evaluated each Render so the
// value always reflects live state (e.g. current in-flight turns). Call once at
// startup.
func RegisterGauge(name, help string, labelNames []string, fn func() []GaugeSample) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.gaugeFuncs = append(reg.gaugeFuncs, gaugeFunc{name: name, help: help, labels: labelNames, fn: fn})
}

// RegisterCounterFunc wires a pull-at-scrape COUNTER: like RegisterGauge, fn is
// evaluated each Render, but the value is emitted with `# TYPE <name> counter`.
// Use this for cumulative monotonic values read live from a source that already
// tracks the running total (e.g. sql.DBStats.WaitCount, #276) — there is no
// per-event delta to push, so incCounter doesn't fit. Call once at startup.
func RegisterCounterFunc(name, help string, labelNames []string, fn func() []GaugeSample) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.counterFuncs = append(reg.counterFuncs, gaugeFunc{name: name, help: help, labels: labelNames, fn: fn})
}

// ── render ───────────────────────────────────────────────────────────────────

// Render returns the full metrics snapshot in Prometheus text exposition format.
func Render() string {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	var b strings.Builder
	names := make([]string, 0, len(reg.counters))
	for n := range reg.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := reg.counters[n]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", n, c.help, n)
		for _, line := range sortedSeries(c.values) {
			fmt.Fprintf(&b, "%s%s %s\n", n, line.labels, formatFloat(line.value))
		}
	}

	hnames := make([]string, 0, len(reg.histograms))
	for n := range reg.histograms {
		hnames = append(hnames, n)
	}
	sort.Strings(hnames)
	for _, n := range hnames {
		h := reg.histograms[n]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", n, h.help, n)
		keys := make([]string, 0, len(h.series))
		for k := range h.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := h.series[k]
			var cumulative uint64
			for i, bound := range h.buckets {
				cumulative += s.counts[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", n, withLE(k, formatFloat(bound)), cumulative)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", n, withLE(k, "+Inf"), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", n, k, formatFloat(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", n, k, s.count)
		}
	}

	gnames := make([]string, 0, len(reg.gauges))
	for n := range reg.gauges {
		gnames = append(gnames, n)
	}
	sort.Strings(gnames)
	for _, n := range gnames {
		g := reg.gauges[n]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", n, g.help, n)
		for _, line := range sortedSeries(g.values) {
			fmt.Fprintf(&b, "%s%s %s\n", n, line.labels, formatFloat(line.value))
		}
	}

	for _, g := range reg.counterFuncs {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", g.name, g.help, g.name)
		for _, s := range g.fn() {
			fmt.Fprintf(&b, "%s%s %s\n", g.name, seriesKey(g.labels, s.Labels), formatFloat(s.Value))
		}
	}

	for _, g := range reg.gaugeFuncs {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, s := range g.fn() {
			fmt.Fprintf(&b, "%s%s %s\n", g.name, seriesKey(g.labels, s.Labels), formatFloat(s.Value))
		}
	}

	return b.String()
}

// ── helpers ──────────────────────────────────────────────────────────────────

type renderedSeries struct {
	labels string
	value  float64
}

func sortedSeries(values map[string]float64) []renderedSeries {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]renderedSeries, 0, len(keys))
	for _, k := range keys {
		out = append(out, renderedSeries{labels: k, value: values[k]})
	}
	return out
}

// seriesKey serializes label name/value pairs into the `{k="v",...}` suffix
// Prometheus expects, escaping values. Empty when there are no labels.
func seriesKey(names, vals []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names))
	for i, n := range names {
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		// %q yields a double-quoted, escaped string — valid Prometheus label-value
		// syntax (escapes ", \, and newlines, which is what Prometheus requires).
		parts = append(parts, fmt.Sprintf("%s=%q", n, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// withLE inserts an le="<bound>" label into an existing `{...}` series key (or
// creates one) for histogram buckets.
func withLE(key, bound string) string {
	le := fmt.Sprintf("le=%q", bound)
	if key == "" {
		return "{" + le + "}"
	}
	return key[:len(key)-1] + "," + le + "}"
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
