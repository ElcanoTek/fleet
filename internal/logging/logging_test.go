package logging

import (
	"bytes"
	"encoding/json"
	"log"
	"log/slog"
	"os"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"INFO":  slog.LevelInfo,
		" warn": slog.LevelWarn,
		"error": slog.LevelError,
		"":      slog.LevelInfo,
		"bogus": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
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
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(newJSONHandler(&buf, slog.LevelInfo)))

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

func TestSetLevel(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(newJSONHandler(&buf, levelVar))
	SetLevel("error")
	lg.Warn("warn-suppressed")
	if buf.Len() != 0 {
		t.Errorf("warn should be suppressed after SetLevel(error), got %q", buf.String())
	}
	SetLevel("debug")
	lg.Debug("debug-shown")
	if !bytes.Contains(buf.Bytes(), []byte("debug-shown")) {
		t.Error("debug should be emitted after SetLevel(debug)")
	}
	SetLevel("info")
}

// TestConfigureJSONToFile exercises the full Configure(json) wiring end-to-end:
// a standard log.Printf line lands in the rotating file as a JSON object. Saves
// and restores the process-global logger/log state so it doesn't leak to other
// tests.
func TestConfigureJSONToFile(t *testing.T) {
	prevSlog := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
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
