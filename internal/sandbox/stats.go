package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Per-task sandbox resource telemetry (#263).
//
// This is OBSERVABILITY only — it samples `podman stats` read-only and never
// touches the container's isolation or resource caps. The data lets operators
// right-size --memory/--cpus/--pids-limit and spot runaway turns; it changes
// nothing about what the sandbox enforces.
//
// Design:
//   - A background goroutine polls `podman stats --no-stream --format json
//     <id>` on a ticker for the lifetime of the container. --no-stream means
//     each poll is a short-lived process we re-invoke per tick, so the
//     goroutine stays cancellable (vs. a long-running stream we'd have to kill)
//     and we don't accumulate a PID inside the container.
//   - Samples feed a rolling aggregator that keeps averages + high-water marks.
//   - On the first poll that crosses the memory-pressure threshold the
//     collector logs one warning (at most once per container) so a turn that
//     brushes its --memory cap is visible without log spam.
//
// Gating: collection is best-effort. A missing podman binary, a stats command
// that errors (container already exited), or unparseable output all DEGRADE to
// "no telemetry for this run" — they never fail the turn. ModeHost spawns no
// goroutine at all (there is no container to query).

// defaultStatsInterval is the poll cadence applied when FLEET_SANDBOX_STATS_INTERVAL_SECONDS
// is unset. minStatsInterval floors it so an operator can't peg a core re-spawning
// `podman stats` every few hundred milliseconds.
const (
	defaultStatsInterval = 10 * time.Second
	minStatsInterval     = 5 * time.Second
)

// memWarnThreshold is the fraction of the memory limit at which the collector
// emits its one-shot "approaching cap" warning (#263 breach detection).
const memWarnThreshold = 0.90

// PodmanStatSample is one decoded `podman stats --format json` row, normalized
// to machine units. Podman renders the on-wire JSON as human strings
// ("503.8kB / 134.2MB", "16.83%", "1"); decoding lives in
// parsePodmanStats, which tolerates both the Podman 5.x (snake_case) and 4.x
// (CamelCase) field shapes — see the issue note on cross-version drift.
type PodmanStatSample struct {
	// At is the host wall-clock time the sample was taken (not from podman;
	// podman stats carries no timestamp).
	At time.Time `json:"at"`

	CPUPercent float64 `json:"cpu_percent"`

	MemUsageBytes uint64 `json:"mem_usage_bytes"`
	MemLimitBytes uint64 `json:"mem_limit_bytes"`

	// BlockInputBytes / BlockOutputBytes are cumulative disk I/O for the
	// container (the "read / write" halves of podman's block_io field).
	BlockInputBytes  uint64 `json:"block_input_bytes"`
	BlockOutputBytes uint64 `json:"block_output_bytes"`

	// NetInputBytes / NetOutputBytes are cumulative network I/O. They are left
	// zero (and excluded from the summary) for NoNetwork containers, whose
	// empty namespace has no meaningful counters to report.
	NetInputBytes  uint64 `json:"net_input_bytes"`
	NetOutputBytes uint64 `json:"net_output_bytes"`

	PidsCurrent uint64 `json:"pids_current"`
}

// ResourceUsageSummary is the per-run rollup the collector produces from the
// stream of samples: averages plus high-water marks for the metrics that have a
// meaningful peak, and the last-seen value for the cumulative I/O counters.
type ResourceUsageSummary struct {
	Samples int `json:"samples"`

	CPUPercentAvg  float64 `json:"cpu_percent_avg"`
	CPUPercentPeak float64 `json:"cpu_percent_peak"`

	MemUsageBytesAvg  uint64 `json:"mem_usage_bytes_avg"`
	MemUsageBytesPeak uint64 `json:"mem_usage_bytes_peak"`
	MemLimitBytes     uint64 `json:"mem_limit_bytes"`

	// BlockInputBytes / BlockOutputBytes / Net* are cumulative counters, so the
	// summary carries the last-seen (highest) value rather than an average.
	BlockInputBytes  uint64 `json:"block_input_bytes"`
	BlockOutputBytes uint64 `json:"block_output_bytes"`

	// NetReported is false for NoNetwork containers; consumers should hide the
	// Net* fields rather than render misleading zeros.
	NetReported    bool   `json:"net_reported"`
	NetInputBytes  uint64 `json:"net_input_bytes,omitempty"`
	NetOutputBytes uint64 `json:"net_output_bytes,omitempty"`

	PidsPeak uint64 `json:"pids_peak"`
}

// statsAggregator folds samples into a ResourceUsageSummary incrementally so we
// never hold the full time-series in memory (a long turn at a 5s cadence is
// thousands of samples). Not goroutine-safe; the collector owns it on one
// goroutine.
type statsAggregator struct {
	count int

	cpuSum  float64
	cpuPeak float64

	memSum   float64 // float to avoid uint64 overflow across many large samples
	memPeak  uint64
	memLast  uint64
	memLimit uint64

	blockIn  uint64
	blockOut uint64

	netIn  uint64
	netOut uint64

	pidsPeak uint64
}

// add folds one sample into the running aggregate.
func (a *statsAggregator) add(s PodmanStatSample) {
	a.count++

	a.cpuSum += s.CPUPercent
	if s.CPUPercent > a.cpuPeak {
		a.cpuPeak = s.CPUPercent
	}

	a.memSum += float64(s.MemUsageBytes)
	if s.MemUsageBytes > a.memPeak {
		a.memPeak = s.MemUsageBytes
	}
	a.memLast = s.MemUsageBytes
	if s.MemLimitBytes > 0 {
		a.memLimit = s.MemLimitBytes
	}

	// Cumulative counters only ever grow within a run; guard against a stray
	// lower reading (e.g. a container restart edge) by taking the max.
	if s.BlockInputBytes > a.blockIn {
		a.blockIn = s.BlockInputBytes
	}
	if s.BlockOutputBytes > a.blockOut {
		a.blockOut = s.BlockOutputBytes
	}
	if s.NetInputBytes > a.netIn {
		a.netIn = s.NetInputBytes
	}
	if s.NetOutputBytes > a.netOut {
		a.netOut = s.NetOutputBytes
	}

	if s.PidsCurrent > a.pidsPeak {
		a.pidsPeak = s.PidsCurrent
	}
}

// summary materializes the rollup. netReported controls whether Net* fields are
// surfaced (false for NoNetwork containers).
func (a *statsAggregator) summary(netReported bool) ResourceUsageSummary {
	if a.count == 0 {
		return ResourceUsageSummary{}
	}
	sum := ResourceUsageSummary{
		Samples:           a.count,
		CPUPercentAvg:     a.cpuSum / float64(a.count),
		CPUPercentPeak:    a.cpuPeak,
		MemUsageBytesAvg:  uint64(a.memSum / float64(a.count)),
		MemUsageBytesPeak: a.memPeak,
		MemLimitBytes:     a.memLimit,
		BlockInputBytes:   a.blockIn,
		BlockOutputBytes:  a.blockOut,
		PidsPeak:          a.pidsPeak,
	}
	if netReported {
		sum.NetReported = true
		sum.NetInputBytes = a.netIn
		sum.NetOutputBytes = a.netOut
	}
	return sum
}

// memBreached reports whether the latest memory reading has crossed
// memWarnThreshold of the limit. Returns false when either value is unknown so
// a missing limit can't trigger a spurious warning.
func (a *statsAggregator) memBreached() bool {
	if a.memLimit == 0 || a.memLast == 0 {
		return false
	}
	return float64(a.memLast) >= float64(a.memLimit)*memWarnThreshold
}

// resolveStatsInterval reads FLEET_SANDBOX_STATS_INTERVAL_SECONDS and clamps it.
// Empty/zero/invalid → defaultStatsInterval; anything below minStatsInterval is
// raised to the floor. A negative value DISABLES collection (returns 0), giving
// operators an explicit off switch for hosts where `podman stats` is costly.
func resolveStatsInterval(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultStatsInterval
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultStatsInterval
	}
	if n < 0 {
		return 0
	}
	d := time.Duration(n) * time.Second
	if d == 0 {
		return defaultStatsInterval
	}
	if d < minStatsInterval {
		return minStatsInterval
	}
	return d
}

// collectStats polls podman stats until ctx is cancelled (the container is
// closing) or the stats command starts failing because the container exited. It
// returns the rolled-up summary so the caller can attach it to the run.
//
// The function blocks; callers run it on a dedicated goroutine and read the
// result via a done channel. It is intentionally tolerant: every error path
// degrades to "no more samples" rather than propagating — telemetry must never
// be able to fail a turn.
func collectStats(ctx context.Context, podman, containerID string, interval time.Duration, netReported bool, onBreach func(usedBytes, limitBytes uint64)) ResourceUsageSummary {
	agg := &statsAggregator{}
	warned := false
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return agg.summary(netReported)
		case <-ticker.C:
			sample, ok := pollOnce(ctx, podman, containerID)
			if !ok {
				// Container likely exited (stats errored) or output was
				// unparseable. Keep ticking — a transient error shouldn't end
				// collection — but if ctx is done we'll exit on the next loop.
				continue
			}
			agg.add(sample)
			if !warned && agg.memBreached() {
				warned = true
				if onBreach != nil {
					onBreach(agg.memLast, agg.memLimit)
				}
			}
		}
	}
}

// pollOnce runs a single `podman stats --no-stream --format json` and returns
// the first decoded sample. ok=false on any error (binary missing, container
// gone, malformed output) so the caller can skip the tick.
func pollOnce(ctx context.Context, podman, containerID string) (PodmanStatSample, bool) {
	//nolint:gosec // podman binary + our generated container ID, no shell, no user input
	cmd := exec.CommandContext(ctx, podman, "stats", "--no-stream", "--format", "json", containerID)
	out, err := cmd.Output()
	if err != nil {
		return PodmanStatSample{}, false
	}
	samples, err := parsePodmanStats(out)
	if err != nil || len(samples) == 0 {
		return PodmanStatSample{}, false
	}
	s := samples[0]
	s.At = time.Now()
	return s, true
}

// rawPodmanStat captures every field name `podman stats --format json` is known
// to emit across the 4.x and 5.x lines, so one decode pass handles both. Each
// value is a string (podman renders human-readable strings, not numbers) which
// the normalizer below converts to machine units.
//
// 5.x (snake_case): cpu_percent, mem_usage ("u / l"), mem_percent, net_io,
//
//	block_io, pids.
//
// 4.x (CamelCase): CPU, MemUsage, MemPerc, NetIO, BlockIO, PIDS.
type rawPodmanStat struct {
	// CPU %
	CPUPercent string `json:"cpu_percent"`
	CPU        string `json:"CPU"`

	// "used / limit"
	MemUsage   string `json:"mem_usage"`
	MemUsageCC string `json:"MemUsage"`

	// "in / out"
	NetIO   string `json:"net_io"`
	NetIOCC string `json:"NetIO"`

	// "read / write"
	BlockIO   string `json:"block_io"`
	BlockIOCC string `json:"BlockIO"`

	Pids   json.Number `json:"pids"`
	PidsCC json.Number `json:"PIDS"`
}

// firstNonEmpty returns the first non-blank string, letting the normalizer pick
// whichever field shape podman actually populated.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parsePodmanStats decodes a `podman stats --format json` array (the command
// always emits a JSON array, even for a single container) into normalized
// samples. Version-tolerant across the Podman 4.x/5.x field-name drift.
func parsePodmanStats(b []byte) ([]PodmanStatSample, error) {
	var raws []rawPodmanStat
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, fmt.Errorf("decode podman stats json: %w", err)
	}
	out := make([]PodmanStatSample, 0, len(raws))
	for _, r := range raws {
		var s PodmanStatSample
		s.CPUPercent = parsePercent(firstNonEmpty(r.CPUPercent, r.CPU))
		s.MemUsageBytes, s.MemLimitBytes = parseUsageLimit(firstNonEmpty(r.MemUsage, r.MemUsageCC))
		s.NetInputBytes, s.NetOutputBytes = parsePair(firstNonEmpty(r.NetIO, r.NetIOCC))
		s.BlockInputBytes, s.BlockOutputBytes = parsePair(firstNonEmpty(r.BlockIO, r.BlockIOCC))
		s.PidsCurrent = parsePids(firstNonEmpty(r.Pids.String(), r.PidsCC.String()))
		out = append(out, s)
	}
	return out, nil
}

// parsePercent turns "16.83%" (or "16.83") into 16.83. Unparseable → 0.
func parsePercent(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// parsePids turns podman's pids field ("1") into a count. Unparseable → 0.
func parsePids(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseUsageLimit splits podman's "used / limit" memory field (e.g.
// "503.8kB / 134.2MB") into (used, limit) bytes. A field with no separator is
// treated as a bare "used" value with an unknown limit.
func parseUsageLimit(s string) (used, limit uint64) {
	left, right, found := splitSlash(s)
	used = parseSize(left)
	if found {
		limit = parseSize(right)
	}
	return used, limit
}

// parsePair splits a "a / b" field (net_io, block_io) into two byte counts.
func parsePair(s string) (a, b uint64) {
	left, right, _ := splitSlash(s)
	return parseSize(left), parseSize(right)
}

// splitSlash splits "a / b" on the first slash, trimming whitespace. found is
// false when there is no slash.
func splitSlash(s string) (left, right string, found bool) {
	idx := strings.IndexByte(s, '/')
	if idx < 0 {
		return strings.TrimSpace(s), "", false
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:]), true
}

// sizeUnits maps podman's human byte suffixes to multipliers. Podman emits
// SI-style decimal suffixes (kB/MB/GB) but some builds/locales use B/KiB-style;
// we accept the common forms. Unknown suffix → treated as bytes.
var sizeUnits = []struct {
	suffix string
	mult   float64
}{
	// Order matters: check longer suffixes first so "kB" doesn't match before "B".
	{"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3},
	{"PiB", 1 << 50}, {"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"B", 1},
}

// parseSize converts a human byte string ("503.8kB", "134.2MB", "0B", "4.096kB")
// to bytes. Unparseable → 0. The decimal/binary distinction follows podman's own
// rendering (it uses decimal kB/MB by default); the small inaccuracy vs. a pure
// cgroup byte count is acceptable for right-sizing telemetry.
func parseSize(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "--" {
		return 0
	}
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0
			}
			if f < 0 {
				return 0
			}
			// Round rather than truncate: "134.2MB" is 134.2*1e6, which
			// floating-point renders as 134199999.999…; a bare uint64() cast
			// would lose a byte. math.Round recovers the intended value.
			return uint64(math.Round(f * u.mult))
		}
	}
	// No recognized suffix: maybe a bare number of bytes.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return uint64(math.Round(f))
}

// logMemoryBreach emits the one-shot "approaching memory cap" warning. Kept as a
// package function so the collector's onBreach closure stays a one-liner and the
// message format is testable/greppable.
func logMemoryBreach(containerID string, usedBytes, limitBytes uint64) {
	if limitBytes == 0 {
		return
	}
	pct := float64(usedBytes) / float64(limitBytes) * 100
	log.Printf("sandbox %s: memory usage at %.0f%% of limit (%s / %s) — consider raising the sandbox memory limit",
		shortID(containerID), pct, humanMiB(usedBytes), humanMiB(limitBytes))
}

// humanMiB renders a byte count as "NNN MiB" for the breach warning.
func humanMiB(b uint64) string {
	return fmt.Sprintf("%d MiB", b/(1<<20))
}

// shortID trims the container name to a readable prefix for log lines.
func shortID(id string) string {
	if len(id) > 24 {
		return id[:24]
	}
	return id
}
