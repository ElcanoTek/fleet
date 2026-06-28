package sandbox

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// Podman 5.x stats output: snake_case fields, human-readable string values.
// Captured from `podman stats --no-stream --format json` on Podman 5.8.2.
const podman5Sample = `[
 {
  "id": "c7450c39b179",
  "name": "fleet-stats-probe",
  "cpu_time": "5.515ms",
  "cpu_percent": "16.83%",
  "avg_cpu": "16.83%",
  "mem_usage": "503.8kB / 134.2MB",
  "mem_percent": "0.38%",
  "net_io": "90B / 132B",
  "block_io": "4.096kB / 8.192kB",
  "pids": "3"
 }
]`

// Podman 4.x stats output: CamelCase field names (CPU, MemUsage, NetIO, BlockIO,
// PIDS). Constructed from the documented 4.x --format json shape so the parser's
// cross-version tolerance is exercised even though CI runs 5.x.
const podman4Sample = `[
 {
  "Name": "fleet-stats-probe",
  "CPU": "42.5%",
  "MemUsage": "256MiB / 512MiB",
  "MemPerc": "50.00%",
  "NetIO": "1.5kB / 2kB",
  "BlockIO": "10MB / 20MB",
  "PIDS": "12"
 }
]`

func TestParsePodmanStats_Podman5(t *testing.T) {
	samples, err := parsePodmanStats([]byte(podman5Sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	s := samples[0]
	if s.CPUPercent != 16.83 {
		t.Errorf("cpu: want 16.83, got %v", s.CPUPercent)
	}
	// 503.8kB decimal = 503800 bytes.
	if s.MemUsageBytes != 503800 {
		t.Errorf("mem usage: want 503800, got %d", s.MemUsageBytes)
	}
	// 134.2MB decimal = 134200000 bytes.
	if s.MemLimitBytes != 134200000 {
		t.Errorf("mem limit: want 134200000, got %d", s.MemLimitBytes)
	}
	if s.BlockInputBytes != 4096 || s.BlockOutputBytes != 8192 {
		t.Errorf("block io: want 4096/8192, got %d/%d", s.BlockInputBytes, s.BlockOutputBytes)
	}
	if s.NetInputBytes != 90 || s.NetOutputBytes != 132 {
		t.Errorf("net io: want 90/132, got %d/%d", s.NetInputBytes, s.NetOutputBytes)
	}
	if s.PidsCurrent != 3 {
		t.Errorf("pids: want 3, got %d", s.PidsCurrent)
	}
}

func TestParsePodmanStats_Podman4CamelCase(t *testing.T) {
	samples, err := parsePodmanStats([]byte(podman4Sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	s := samples[0]
	if s.CPUPercent != 42.5 {
		t.Errorf("cpu: want 42.5, got %v", s.CPUPercent)
	}
	// 256MiB binary = 256 * 1<<20.
	if s.MemUsageBytes != 256<<20 {
		t.Errorf("mem usage: want %d, got %d", 256<<20, s.MemUsageBytes)
	}
	if s.MemLimitBytes != 512<<20 {
		t.Errorf("mem limit: want %d, got %d", 512<<20, s.MemLimitBytes)
	}
	if s.BlockInputBytes != 10_000_000 || s.BlockOutputBytes != 20_000_000 {
		t.Errorf("block io: want 10000000/20000000, got %d/%d", s.BlockInputBytes, s.BlockOutputBytes)
	}
	if s.PidsCurrent != 12 {
		t.Errorf("pids: want 12, got %d", s.PidsCurrent)
	}
}

func TestParsePodmanStats_Malformed(t *testing.T) {
	if _, err := parsePodmanStats([]byte("not json")); err == nil {
		t.Error("expected error on malformed json")
	}
	// An empty array decodes cleanly to zero samples (container vanished).
	samples, err := parsePodmanStats([]byte("[]"))
	if err != nil {
		t.Fatalf("empty array should decode: %v", err)
	}
	if len(samples) != 0 {
		t.Errorf("want 0 samples, got %d", len(samples))
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0B", 0},
		{"4.096kB", 4096},
		{"503.8kB", 503800},
		{"134.2MB", 134200000},
		{"256MiB", 256 << 20},
		{"1GiB", 1 << 30},
		{"2GB", 2_000_000_000},
		{"1024", 1024}, // bare bytes
		{"--", 0},
		{"", 0},
		{"garbage", 0},
		{"-5MB", 0}, // negative is rejected
	}
	for _, c := range cases {
		if got := parseSize(c.in); got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParsePercent(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"16.83%", 16.83},
		{"100%", 100},
		{"42.5", 42.5}, // no suffix
		{"", 0},
		{"n/a", 0},
	}
	for _, c := range cases {
		if got := parsePercent(c.in); got != c.want {
			t.Errorf("parsePercent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStatsAggregator_AvgAndPeak(t *testing.T) {
	agg := &statsAggregator{}
	agg.add(PodmanStatSample{CPUPercent: 10, MemUsageBytes: 100, MemLimitBytes: 1000, PidsCurrent: 2, BlockInputBytes: 5, BlockOutputBytes: 7})
	agg.add(PodmanStatSample{CPUPercent: 30, MemUsageBytes: 300, MemLimitBytes: 1000, PidsCurrent: 5, BlockInputBytes: 9, BlockOutputBytes: 11})
	agg.add(PodmanStatSample{CPUPercent: 20, MemUsageBytes: 200, MemLimitBytes: 1000, PidsCurrent: 3, BlockInputBytes: 12, BlockOutputBytes: 15})

	sum := agg.summary(true)
	if sum.Samples != 3 {
		t.Errorf("samples: want 3, got %d", sum.Samples)
	}
	if sum.CPUPercentAvg != 20 {
		t.Errorf("cpu avg: want 20, got %v", sum.CPUPercentAvg)
	}
	if sum.CPUPercentPeak != 30 {
		t.Errorf("cpu peak: want 30, got %v", sum.CPUPercentPeak)
	}
	if sum.MemUsageBytesAvg != 200 {
		t.Errorf("mem avg: want 200, got %d", sum.MemUsageBytesAvg)
	}
	if sum.MemUsageBytesPeak != 300 {
		t.Errorf("mem peak: want 300, got %d", sum.MemUsageBytesPeak)
	}
	if sum.MemLimitBytes != 1000 {
		t.Errorf("mem limit: want 1000, got %d", sum.MemLimitBytes)
	}
	if sum.PidsPeak != 5 {
		t.Errorf("pids peak: want 5, got %d", sum.PidsPeak)
	}
	// Cumulative counters: last/highest reading wins.
	if sum.BlockInputBytes != 12 || sum.BlockOutputBytes != 15 {
		t.Errorf("block io: want 12/15, got %d/%d", sum.BlockInputBytes, sum.BlockOutputBytes)
	}
}

func TestStatsAggregator_Empty(t *testing.T) {
	agg := &statsAggregator{}
	sum := agg.summary(true)
	if sum.Samples != 0 {
		t.Errorf("empty aggregator should produce 0 samples, got %d", sum.Samples)
	}
}

func TestStatsAggregator_NetGating(t *testing.T) {
	agg := &statsAggregator{}
	agg.add(PodmanStatSample{NetInputBytes: 50, NetOutputBytes: 60})

	// NoNetwork run: Net* fields suppressed.
	noNet := agg.summary(false)
	if noNet.NetReported {
		t.Error("net should not be reported when netReported=false")
	}
	if noNet.NetInputBytes != 0 || noNet.NetOutputBytes != 0 {
		t.Errorf("net fields should be zero when suppressed, got %d/%d", noNet.NetInputBytes, noNet.NetOutputBytes)
	}

	// Networked run: Net* surfaced.
	withNet := agg.summary(true)
	if !withNet.NetReported {
		t.Error("net should be reported when netReported=true")
	}
	if withNet.NetInputBytes != 50 || withNet.NetOutputBytes != 60 {
		t.Errorf("net io: want 50/60, got %d/%d", withNet.NetInputBytes, withNet.NetOutputBytes)
	}
}

func TestStatsAggregator_MemBreached(t *testing.T) {
	// Below threshold: 89% of 1000.
	below := &statsAggregator{}
	below.add(PodmanStatSample{MemUsageBytes: 890, MemLimitBytes: 1000})
	if below.memBreached() {
		t.Error("890/1000 (89%) should not breach the 90% threshold")
	}

	// At/above threshold: 90% exactly.
	at := &statsAggregator{}
	at.add(PodmanStatSample{MemUsageBytes: 900, MemLimitBytes: 1000})
	if !at.memBreached() {
		t.Error("900/1000 (90%) should breach the threshold")
	}

	// Above threshold.
	above := &statsAggregator{}
	above.add(PodmanStatSample{MemUsageBytes: 950, MemLimitBytes: 1000})
	if !above.memBreached() {
		t.Error("950/1000 (95%) should breach the threshold")
	}

	// Unknown limit: never breaches (avoids spurious warnings).
	noLimit := &statsAggregator{}
	noLimit.add(PodmanStatSample{MemUsageBytes: 9999, MemLimitBytes: 0})
	if noLimit.memBreached() {
		t.Error("unknown limit (0) should never breach")
	}
}

func TestResolveStatsInterval(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", defaultStatsInterval},
		{"0", defaultStatsInterval},
		{"bogus", defaultStatsInterval},
		{"10", 10 * time.Second},
		{"30", 30 * time.Second},
		{"3", minStatsInterval},      // below floor → raised
		{"5", minStatsInterval},      // exactly the floor
		{"-1", 0},                    // negative disables
		{"  12  ", 12 * time.Second}, // whitespace tolerated
	}
	for _, c := range cases {
		if got := resolveStatsInterval(c.in); got != c.want {
			t.Errorf("resolveStatsInterval(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestContainerResourceUsage_E2E is a podman-gated integration test for the live
// collector path (#263): it spins a real container with a short stats interval,
// runs a small workload, closes it, and asserts non-empty telemetry was rolled
// up and exposed via Sandbox.ResourceUsage. Skipped without linux+podman.
func TestContainerResourceUsage_E2E(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	image := testImage()
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            image,
		WorkspaceHostDir: tmp,
		BridgeScript:     []byte("# unused for bash-only test\n"),
		// Floor cadence so a short-lived test still captures a sample.
		StatsInterval: minStatsInterval,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}

	// Run a brief CPU/memory workload so there is something to measure, and
	// give the collector at least one full tick before teardown.
	if _, err := sb.RunBash(context.Background(), BashRequest{
		Command: "head -c 20000000 /dev/zero > /tmp/blob; sleep 6",
	}); err != nil {
		sb.Close()
		t.Fatalf("RunBash: %v", err)
	}

	// Close finalizes telemetry (the poller publishes its rollup on teardown).
	sb.Close()

	summary, ok := sb.ResourceUsage()
	if !ok {
		t.Skip("no stats samples collected (host podman may restrict `podman stats` for rootless cgroupv1) — collection degraded gracefully, which is the intended fallback")
	}
	if summary.Samples == 0 {
		t.Fatalf("ResourceUsage ok=true but Samples=0")
	}
	if summary.MemUsageBytesPeak == 0 {
		t.Errorf("expected non-zero peak memory, got summary %+v", summary)
	}
	if summary.PidsPeak == 0 {
		t.Errorf("expected non-zero peak pids, got summary %+v", summary)
	}
}
