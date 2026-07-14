// Package hoststats collects a small, read-only Linux host snapshot for the
// admin settings UI. It deliberately uses procfs + statfs instead of a daemon
// or third-party metrics dependency: this is a convenient operator glance, not
// a replacement for Prometheus/node_exporter.
package hoststats

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type CPU struct {
	Available    bool     `json:"available"`
	Cores        int      `json:"cores"`
	UsagePercent *float64 `json:"usage_percent"`
	Load1        float64  `json:"load_1"`
	Load5        float64  `json:"load_5"`
	Load15       float64  `json:"load_15"`
}

type Memory struct {
	Available      bool   `json:"available"`
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapUsedBytes  uint64 `json:"swap_used_bytes"`
}

type Disk struct {
	Available      bool    `json:"available"`
	Path           string  `json:"path"`
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsagePercent   float64 `json:"usage_percent"`
}

type Network struct {
	Available        bool     `json:"available"`
	Interfaces       int      `json:"interfaces"`
	ReceivedBytes    uint64   `json:"received_bytes"`
	TransmittedBytes uint64   `json:"transmitted_bytes"`
	ReceiveBPS       *float64 `json:"receive_bytes_per_second"`
	TransmitBPS      *float64 `json:"transmit_bytes_per_second"`
}

type Snapshot struct {
	Available     bool      `json:"available"`
	SampledAt     time.Time `json:"sampled_at"`
	Hostname      string    `json:"hostname,omitempty"`
	Platform      string    `json:"platform"`
	UptimeSeconds float64   `json:"uptime_seconds"`
	CPU           CPU       `json:"cpu"`
	Memory        Memory    `json:"memory"`
	Disk          Disk      `json:"disk"`
	Network       Network   `json:"network"`
	Warnings      []string  `json:"warnings,omitempty"`
}

type Collector struct {
	mu sync.Mutex

	haveCPU  bool
	cpuTotal uint64
	cpuIdle  uint64

	haveNetwork bool
	networkAt   time.Time
	networkRX   uint64
	networkTX   uint64

	procRoot string
	diskPath string
	now      func() time.Time
}

func New() *Collector {
	return &Collector{procRoot: "/proc", diskPath: "/", now: time.Now}
}

// Collect returns every section it can read. A missing procfs (for example on
// a developer laptop) produces a partial snapshot plus warnings instead of a
// failing endpoint; the UI labels unavailable sections explicitly.
func (c *Collector) Collect() Snapshot {
	if c == nil {
		return Snapshot{SampledAt: time.Now().UTC(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Warnings: []string{"collector unavailable"}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	out := Snapshot{SampledAt: now.UTC(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Warnings: []string{}}
	out.Hostname, _ = os.Hostname()

	if uptime, err := readUptime(c.procRoot + "/uptime"); err == nil {
		out.UptimeSeconds = uptime
	} else {
		out.Warnings = append(out.Warnings, "host uptime unavailable")
	}

	total, idle, err := readCPU(c.procRoot + "/stat")
	out.CPU.Cores = runtime.NumCPU()
	if err == nil {
		out.CPU.Available = true
		if c.haveCPU && total > c.cpuTotal && idle >= c.cpuIdle {
			deltaTotal := total - c.cpuTotal
			deltaIdle := idle - c.cpuIdle
			if deltaIdle <= deltaTotal {
				usage := 100 * float64(deltaTotal-deltaIdle) / float64(deltaTotal)
				usage = clampPercent(usage)
				out.CPU.UsagePercent = &usage
			}
		}
		c.haveCPU, c.cpuTotal, c.cpuIdle = true, total, idle
		if l1, l5, l15, loadErr := readLoad(c.procRoot + "/loadavg"); loadErr == nil {
			out.CPU.Load1, out.CPU.Load5, out.CPU.Load15 = l1, l5, l15
		}
	} else {
		out.Warnings = append(out.Warnings, "CPU statistics unavailable")
	}

	if mem, err := readMemory(c.procRoot + "/meminfo"); err == nil {
		out.Memory = mem
	} else {
		out.Warnings = append(out.Warnings, "memory statistics unavailable")
	}

	if disk, err := readDisk(c.diskPath); err == nil {
		out.Disk = disk
	} else {
		out.Warnings = append(out.Warnings, "disk statistics unavailable")
	}

	if rx, tx, interfaces, err := readNetwork(c.procRoot + "/net/dev"); err == nil {
		out.Network = Network{Available: true, Interfaces: interfaces, ReceivedBytes: rx, TransmittedBytes: tx}
		if c.haveNetwork {
			seconds := now.Sub(c.networkAt).Seconds()
			if seconds > 0 && rx >= c.networkRX && tx >= c.networkTX {
				rxRate := float64(rx-c.networkRX) / seconds
				txRate := float64(tx-c.networkTX) / seconds
				out.Network.ReceiveBPS, out.Network.TransmitBPS = &rxRate, &txRate
			}
		}
		c.haveNetwork, c.networkAt, c.networkRX, c.networkTX = true, now, rx, tx
	} else {
		out.Warnings = append(out.Warnings, "network statistics unavailable")
	}

	out.Available = out.CPU.Available || out.Memory.Available || out.Disk.Available || out.Network.Available
	return out
}

func readUptime(path string) (float64, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed procfs path (test collector overrides its root).
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty uptime")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func readCPU(path string) (total, idle uint64, err error) {
	f, err := os.Open(path) // #nosec G304 -- fixed procfs path (test collector overrides its root).
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		return 0, 0, fmt.Errorf("empty cpu stat")
	}
	fields := strings.Fields(s.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("invalid cpu stat")
	}
	values := make([]uint64, 0, len(fields)-1)
	for _, field := range fields[1:] {
		v, parseErr := strconv.ParseUint(field, 10, 64)
		if parseErr != nil {
			return 0, 0, parseErr
		}
		values = append(values, v)
	}
	// Linux cpu totals count user..steal. guest/guest_nice are already included
	// in user/nice and must not be added a second time.
	limit := len(values)
	if limit > 8 {
		limit = 8
	}
	for _, v := range values[:limit] {
		total += v
	}
	idle = values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return total, idle, nil
}

func readLoad(path string) (float64, float64, float64, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed procfs path.
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("invalid loadavg")
	}
	values := [3]float64{}
	for i := range values {
		values[i], err = strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	return values[0], values[1], values[2], nil
}

func readMemory(path string) (Memory, error) {
	f, err := os.Open(path) // #nosec G304 -- fixed procfs path.
	if err != nil {
		return Memory{}, err
	}
	defer f.Close()
	values := map[string]uint64{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	if err := s.Err(); err != nil {
		return Memory{}, err
	}
	total := values["MemTotal"]
	if total == 0 {
		return Memory{}, fmt.Errorf("MemTotal missing")
	}
	available := values["MemAvailable"]
	if available == 0 {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if available > total {
		available = total
	}
	swapTotal, swapFree := values["SwapTotal"], values["SwapFree"]
	if swapFree > swapTotal {
		swapFree = swapTotal
	}
	return Memory{Available: true, TotalBytes: total, UsedBytes: total - available, AvailableBytes: available, SwapTotalBytes: swapTotal, SwapUsedBytes: swapTotal - swapFree}, nil
}

func readDisk(path string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, err
	}
	blockSize := uint64(st.Bsize) // #nosec G115 -- kernel block sizes are non-negative and bounded.
	total := st.Blocks * blockSize
	available := st.Bavail * blockSize
	used := total - st.Bfree*blockSize
	usage := 0.0
	if total > 0 {
		usage = 100 * float64(used) / float64(total)
	}
	return Disk{Available: true, Path: path, TotalBytes: total, UsedBytes: used, AvailableBytes: available, UsagePercent: clampPercent(usage)}, nil
}

func readNetwork(path string) (rx, tx uint64, interfaces int, err error) {
	f, err := os.Open(path) // #nosec G304 -- fixed procfs path.
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		received, rxErr := strconv.ParseUint(fields[0], 10, 64)
		transmitted, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			continue
		}
		rx, tx, interfaces = rx+received, tx+transmitted, interfaces+1
	}
	if err := s.Err(); err != nil {
		return 0, 0, 0, err
	}
	return rx, tx, interfaces, nil
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
