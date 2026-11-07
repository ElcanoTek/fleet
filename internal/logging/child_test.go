package logging

import (
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// A child process's stderr (the fleet mcp-broker's log lines) reaches the
// parent's log destination, FLEET_LOG_FILE included, one whole line at a time
// however the pipe splits the bytes.
func TestChildStderrForwardsWholeLinesToTheLogFile(t *testing.T) {
	prevSlog := slog.Default()
	prevBridge := bridgeLogger
	prevOut := currentOutput()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		bridgeLogger = prevBridge
		setOutput(prevOut)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "json", Level: "info"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	defer closer.Close()

	w := ChildStderr()
	for _, chunk := range []string{`{"msg":"mcpbroker: call`, ` failed","op":"tool call"}` + "\n" + `{"msg":"sec`, `ond"}` + "\n" + `{"msg":"partial`} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"msg":"mcpbroker: call failed","op":"tool call"}` + "\n" + `{"msg":"second"}` + "\n"
	if !strings.HasSuffix(string(data), want) {
		t.Fatalf("log file = %q, want it to end with the two whole child lines %q", data, want)
	}
	if strings.Contains(string(data), "partial") {
		t.Fatalf("an unterminated child line was written: %q", data)
	}
}
