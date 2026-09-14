package mcp

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcpoauth"
)

// TestProbeProtocolVersionMatchesTransport holds the OAuth discovery probe's
// announced MCP revision equal to the one this transport really sends, so a
// server that routes or gates initialization on the offered revision answers
// the probe exactly as it will answer the connection the probe validates.
// mcpoauth cannot import this package (it is reached from here through
// internal/a2a), which is why the pin lives on this side.
func TestProbeProtocolVersionMatchesTransport(t *testing.T) {
	if mcpoauth.ProbeProtocolVersion != mcpProtocolVersion {
		t.Fatalf("mcpoauth.ProbeProtocolVersion = %q, transport sends %q; keep them equal", mcpoauth.ProbeProtocolVersion, mcpProtocolVersion)
	}
}
