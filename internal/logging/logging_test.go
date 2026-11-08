package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{" warn", slog.LevelWarn}, // leading space trimmed
		{"error", slog.LevelError},
		{"", slog.LevelInfo},      // default
		{"bogus", slog.LevelInfo}, // default
	}
	for _, c := range cases {
		if got := ParseLevel(c.in); got != c.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func decodeLast(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var m map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &m); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", lines[len(lines)-1], err)
	}
	return m
}

func TestJSONHandlerEmitsStructured(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(newJSONHandler(&buf, slog.LevelInfo))
	lg.Info("hello world", "user", "a@b.co")
	m := decodeLast(t, &buf)
	if m["msg"] != "hello world" {
		t.Errorf("msg = %v", m["msg"])
	}
	if m["level"] != "INFO" {
		t.Errorf("level = %v", m["level"])
	}
	if m["user"] != "a@b.co" {
		t.Errorf("non-secret attr should pass through, got user=%v", m["user"])
	}
}

func TestRedactingHandler(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(newJSONHandler(&buf, slog.LevelDebug))
	lg.Info("call",
		"api_token", "sk-supersecret",
		"Authorization", "Bearer abc",
		"user_email", "a@b.co",
		slog.Group("mcp", "server_token", "xyz", "server", "gamma"),
	)
	m := decodeLast(t, &buf)
	if m["api_token"] != "[REDACTED]" {
		t.Errorf("api_token not redacted: %v", m["api_token"])
	}
	if m["Authorization"] != "[REDACTED]" {
		t.Errorf("Authorization not redacted: %v", m["Authorization"])
	}
	if m["user_email"] != "a@b.co" {
		t.Errorf("user_email wrongly redacted: %v", m["user_email"])
	}
	grp, ok := m["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp group missing: %v", m["mcp"])
	}
	if grp["server_token"] != "[REDACTED]" {
		t.Errorf("nested server_token not redacted: %v", grp["server_token"])
	}
	if grp["server"] != "gamma" {
		t.Errorf("nested non-secret wrongly redacted: %v", grp["server"])
	}
	if bytes.Contains(buf.Bytes(), []byte("sk-supersecret")) {
		t.Error("secret value leaked into the log output")
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(newJSONHandler(&buf, slog.LevelWarn))
	lg.Info("suppressed")
	if buf.Len() != 0 {
		t.Errorf("info should be suppressed at warn level, got %q", buf.String())
	}
	lg.Warn("shown")
	if !bytes.Contains(buf.Bytes(), []byte("shown")) {
		t.Error("warn should be emitted at warn level")
	}
}

func TestLogBridge(t *testing.T) {
	prev := bridgeLogger
	defer func() { bridgeLogger = prev }()
	var buf bytes.Buffer
	bridgeLogger = slog.New(newJSONHandler(&buf, slog.LevelInfo))

	if _, err := (logBridge{}).Write([]byte("legacy line via log.Printf\n")); err != nil {
		t.Fatalf("bridge write: %v", err)
	}
	m := decodeLast(t, &buf)
	if m["msg"] != "legacy line via log.Printf" {
		t.Errorf("bridged msg = %v", m["msg"])
	}
	if m["level"] != "INFO" {
		t.Errorf("bridged level = %v", m["level"])
	}
}

// TestLogBridgeFlattensNewlines guards the log-injection fix: a CR/LF smuggled
// into a legacy log.Printf argument must not survive into the bridged message,
// so it cannot forge a second log record for a downstream line-based consumer.
func TestLogBridgeFlattensNewlines(t *testing.T) {
	prev := bridgeLogger
	defer func() { bridgeLogger = prev }()
	var buf bytes.Buffer
	bridgeLogger = slog.New(newJSONHandler(&buf, slog.LevelInfo))

	if _, err := (logBridge{}).Write([]byte("upload \"a.txt\r\nlevel=ERROR forged\"\r\n")); err != nil {
		t.Fatalf("bridge write: %v", err)
	}
	m := decodeLast(t, &buf)
	if got, want := m["msg"], "upload \"a.txt  level=ERROR forged\""; got != want {
		t.Errorf("bridged msg = %q, want %q", got, want)
	}
}

// TestBridgeNotGatedByLevel is the regression guard for the adversarial-review
// finding: legacy bridged lines must ALWAYS emit, even when FLEET_LOG_LEVEL is
// raised to error — otherwise raising the level would silently erase the entire
// (currently un-converted) legacy diagnostic stream.
func TestBridgeNotGatedByLevel(t *testing.T) {
	prev := bridgeLogger
	defer func() { bridgeLogger = prev }()
	var buf bytes.Buffer
	bridgeLogger = slog.New(newJSONHandler(&buf, slog.LevelInfo))

	levelVar.Set(slog.LevelError) // raise the NATIVE slog level
	defer levelVar.Set(slog.LevelInfo)
	if _, err := (logBridge{}).Write([]byte("scheduler: Error recovering expired leases\n")); err != nil {
		t.Fatalf("bridge write: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("legacy bridged line must still emit at FLEET_LOG_LEVEL=error (no silent diagnostic loss)")
	}
	if m := decodeLast(t, &buf); m["msg"] != "scheduler: Error recovering expired leases" {
		t.Errorf("bridged msg = %v", m["msg"])
	}
}

func TestLevelVarGatesJSONHandler(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(newJSONHandler(&buf, levelVar))
	defer levelVar.Set(slog.LevelInfo)
	levelVar.Set(ParseLevel("error"))
	lg.Warn("warn-suppressed")
	if buf.Len() != 0 {
		t.Errorf("warn should be suppressed at level error, got %q", buf.String())
	}
	levelVar.Set(ParseLevel("debug"))
	lg.Debug("debug-shown")
	if !bytes.Contains(buf.Bytes(), []byte("debug-shown")) {
		t.Error("debug should be emitted at level debug")
	}
}

// TestConfigureJSONToFile exercises the full Configure(json) wiring end-to-end:
// a standard log.Printf line lands in the rotating file as a JSON object. Saves
// and restores the process-global logger/log state so it doesn't leak to other
// tests.
func TestConfigureJSONToFile(t *testing.T) {
	prevSlog := slog.Default()
	prevBridge := bridgeLogger
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		bridgeLogger = prevBridge
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	dir := t.TempDir()
	path := dir + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "json", Level: "info"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if closer != nil {
		defer closer.Close()
	}

	log.Printf("startup diagnostic user=%s", "a@b.co")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	var m map[string]any
	last := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(last) == 0 || len(last[len(last)-1]) == 0 {
		t.Fatalf("no log lines written to %s", path)
	}
	if err := json.Unmarshal(last[len(last)-1], &m); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", last[len(last)-1], err)
	}
	if m["msg"] != "startup diagnostic user=a@b.co" {
		t.Errorf("bridged msg = %v", m["msg"])
	}
}

// TestConfigureTextPreservesLegacy verifies Format=text keeps the legacy
// plaintext lines (not JSON).
func TestConfigureTextPreservesLegacy(t *testing.T) {
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
	dir := t.TempDir()
	path := dir + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "text"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if closer != nil {
		defer closer.Close()
	}
	log.Printf("legacy line")
	data, _ := os.ReadFile(path)
	if json.Valid(bytes.TrimSpace(data)) {
		t.Errorf("text format should NOT be JSON, got %q", data)
	}
	if !bytes.Contains(data, []byte("legacy line")) {
		t.Errorf("text format should contain the raw line, got %q", data)
	}
}

// A stdlib log line written before Configure (a library's log.Printf while
// fleet serve is still loading its config) is buffered as a bridged line and
// replayed through the ungated bridge, as it would be after Configure — not
// as an info record that FLEET_LOG_LEVEL=error drops (#2074 review).
func TestStdlibLogBeforeConfigureIsNotGated(t *testing.T) {
	restoreLogging(t)
	BufferStartup()
	log.Printf("library says %s", "hello before configure")
	slog.Info("an ordinary startup info line")

	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "json", Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"msg":"library says hello before configure"`)) {
		t.Fatalf("the buffered stdlib log line was gated away:\n%s", data)
	}
	if bytes.Contains(data, []byte("an ordinary startup info line")) {
		t.Fatalf("an ordinary info line passed level=error:\n%s", data)
	}
}

// TestMain re-execs this test binary as a fleet-mcp-broker-like child when
// FLEET_LOGGING_TEST_CHILD is set: it configures logging as the broker does
// (JSON on stderr, the parent's level) and writes one stdlib log line, one
// ordinary info record and a stdlib_log spoof attempt, then exits.
func TestMain(m *testing.M) {
	if os.Getenv("FLEET_LOGGING_TEST_CHILD") == "1" {
		if _, err := Configure(Config{Format: "json", Level: os.Getenv("FLEET_LOGGING_TEST_LEVEL")}); err != nil {
			os.Exit(2)
		}
		log.Printf("library diagnostic from the child")
		slog.Info("ordinary child info record")
		slog.Info("spoofed stdlib line", StdlibLogKey, true)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A stdlib log line inside a child (the broker's dependency printing a
// failure) stays ungated across the process boundary: the child's bridge
// tags it, and the parent re-logs it through its own ungated bridge, so it
// reaches FLEET_LOG_FILE at FLEET_LOG_LEVEL=error, while an ordinary child
// info record — and a call site trying to spoof the tag — are still gated
// (#2074 review).
func TestChildStdlibLogSurvivesTheParentsLevel(t *testing.T) {
	restoreLogging(t)
	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "json", Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FLEET_LOGGING_TEST_CHILD=1", "FLEET_LOGGING_TEST_LEVEL=info")
	stderr := ChildStderr("mcp-broker")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v", err)
	}
	_ = stderr.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch m["msg"] {
		case "library diagnostic from the child":
			found = m["child"] == "mcp-broker" && m[StdlibLogKey] == true
		case "ordinary child info record", "spoofed stdlib line":
			t.Fatalf("a gated child record passed level=error: %s", line)
		}
	}
	if !found {
		t.Fatalf("the child's stdlib log line did not reach the file at level=error:\n%s", data)
	}
}

// A stdlib log line written while Configure runs is never lost: one that
// reaches the startup bridge after the replay's final read of the buffer is
// forwarded (the buffer is sealed), not stranded in it (#2074 review).
func TestConfigureLosesNoConcurrentStdlibLine(t *testing.T) {
	restoreLogging(t)
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	prevStderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = prevStderr; _ = devnull.Close() }()

	BufferStartup()
	// A log.Printf that loaded the startup bridge just before Configure's
	// log.SetOutput writes through it just after the replay: stand in for it
	// deterministically by keeping that writer and writing through it after
	// Configure returns. The goroutine adds a real concurrent writer.
	inFlight := log.New(log.Writer(), "", 0)
	const n = 1500 // under maxStartupRecords: nothing is dropped by the cap
	var written atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			log.Printf("concurrent line %d.", i)
			written.Add(1)
		}
	}()
	for written.Load() < 50 {
		runtime.Gosched()
	}
	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "json", Level: "info"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	defer closer.Close()
	inFlight.Printf("in-flight line.")
	<-done
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if !bytes.Contains(data, []byte(fmt.Sprintf(`"concurrent line %d."`, i))) {
			t.Fatalf("line %d was lost across Configure (wrote %d)", i, written.Load())
		}
	}
	if !bytes.Contains(data, []byte(`"in-flight line."`)) {
		t.Fatal("a line written through the startup bridge after the replay was lost")
	}
}
