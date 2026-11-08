package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// The broker child logs JSON (the wire format the parent parses and re-logs
// in the operator's format) at the parent's level, and opens no log file of
// its own (the parent owns FLEET_LOG_FILE and its rotation).
func TestMCPBrokerLogConfigFollowsTheParent(t *testing.T) {
	t.Setenv("FLEET_LOG_FORMAT", "text")
	t.Setenv("FLEET_LOG_LEVEL", "warn")
	t.Setenv("FLEET_LOG_FILE", "/var/log/fleet/fleet.log")

	got := mcpBrokerLogConfig()
	if got.Format != "json" || got.Level != "warn" {
		t.Fatalf("broker log config = %+v, want format=json level=warn", got)
	}
	if got.File != "" {
		t.Fatalf("broker log config opens its own file %q; the parent owns FLEET_LOG_FILE", got.File)
	}
}

// SIGTERM ends the broker's Serve (its context), so the deferred span flush
// runs instead of the process dying mid-flush.
func TestMCPBrokerServeContextEndsOnSIGTERM(t *testing.T) {
	ctx, stop := mcpBrokerServeContext()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not end the broker's serve context")
	}
}

// The parent pipes the broker's stderr into its own log destination.
func TestMCPBrokerCommandPipesStderrIntoTheParentLog(t *testing.T) {
	cmd, err := productionMCPBrokerCommand()
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Stderr == nil {
		t.Fatal("broker stderr is not captured")
	}
	if _, isFile := cmd.Stderr.(interface{ Fd() uintptr }); isFile {
		t.Fatal("broker stderr goes straight to a file descriptor, bypassing FLEET_LOG_FILE")
	}
}
