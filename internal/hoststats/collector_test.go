package hoststats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectorReadsHostSnapshotAndCalculatesRates(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("uptime", "12345.5 0\n")
	write("loadavg", "0.25 0.50 0.75 1/100 123\n")
	write("meminfo", "MemTotal: 1000 kB\nMemAvailable: 400 kB\nSwapTotal: 200 kB\nSwapFree: 50 kB\n")
	write("stat", "cpu 100 0 100 500 100 50 50 100 0 0\n")
	write("net/dev", "Inter-| Receive | Transmit\n lo: 10 0 0 0 0 0 0 0 20 0 0 0 0 0 0 0\n eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n")

	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	c := &Collector{procRoot: root, diskPath: root, now: func() time.Time { return now }}
	first := c.Collect()
	if !first.Available || !first.CPU.Available || !first.Memory.Available || !first.Disk.Available || !first.Network.Available {
		t.Fatalf("incomplete first snapshot: %+v", first)
	}
	if first.CPU.UsagePercent != nil || first.Network.ReceiveBPS != nil {
		t.Fatalf("first sample should not invent rates: cpu=%v net=%v", first.CPU.UsagePercent, first.Network.ReceiveBPS)
	}
	if first.Memory.TotalBytes != 1000*1024 || first.Memory.UsedBytes != 600*1024 || first.Memory.SwapUsedBytes != 150*1024 {
		t.Errorf("memory = %+v", first.Memory)
	}
	if first.Network.ReceivedBytes != 1000 || first.Network.TransmittedBytes != 2000 || first.Network.Interfaces != 1 {
		t.Errorf("network = %+v", first.Network)
	}
	if first.UptimeSeconds != 12345.5 || first.CPU.Load15 != 0.75 {
		t.Errorf("uptime/load = %v / %+v", first.UptimeSeconds, first.CPU)
	}

	now = now.Add(2 * time.Second)
	write("stat", "cpu 150 0 150 550 100 100 50 100 0 0\n")
	write("net/dev", "Inter-| Receive | Transmit\n eth0: 1600 0 0 0 0 0 0 0 2600 0 0 0 0 0 0 0\n")
	second := c.Collect()
	if second.CPU.UsagePercent == nil || *second.CPU.UsagePercent != 75 {
		t.Errorf("cpu usage = %v, want 75", second.CPU.UsagePercent)
	}
	if second.Network.ReceiveBPS == nil || *second.Network.ReceiveBPS != 300 || second.Network.TransmitBPS == nil || *second.Network.TransmitBPS != 300 {
		t.Errorf("network rates = %+v, want 300/300", second.Network)
	}

	// Counter resets can occur across host suspend, namespace replacement, or
	// collector fixture changes. Report a sampling gap instead of underflowing.
	now = now.Add(2 * time.Second)
	write("stat", "cpu 1 0 1 1 0 0 0 0 0 0\n")
	write("net/dev", "Inter-| Receive | Transmit\n eth0: 1 0 0 0 0 0 0 0 1 0 0 0 0 0 0 0\n")
	third := c.Collect()
	if third.CPU.UsagePercent != nil || third.Network.ReceiveBPS != nil || third.Network.TransmitBPS != nil {
		t.Errorf("counter reset should leave rates unsampled: cpu=%v net=%+v", third.CPU.UsagePercent, third.Network)
	}
}

func TestCollectorDegradesWhenProcfsIsUnavailable(t *testing.T) {
	c := &Collector{procRoot: filepath.Join(t.TempDir(), "missing"), diskPath: t.TempDir(), now: time.Now}
	got := c.Collect()
	if !got.Disk.Available || got.CPU.Available || got.Memory.Available || got.Network.Available {
		t.Fatalf("partial snapshot = %+v", got)
	}
	if len(got.Warnings) < 3 {
		t.Fatalf("warnings = %v", got.Warnings)
	}
}
