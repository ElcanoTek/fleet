package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// #834: the creds writer and THIS loader are the two ends of the round-trip
// (fleet mcp account writes, the server boot reads). Values the writer quotes
// (inline-comment sequences, literal quotes, edge whitespace) must come back
// byte-identical through the server's parser, not just the creds parser.
func TestLoadEnvFileRoundTripsCredsWriter(t *testing.T) {
	clearEnvVars()
	defer clearEnvVars()
	t.Cleanup(func() { os.Unsetenv("TAVILY_API_KEY") })

	for _, value := range []string{
		"plain-value-123",
		"value-xyz #looks like a comment",
		`"quoted-secret"`,
		"  padded  ",
		"it's got a quote",
	} {
		path := filepath.Join(t.TempDir(), "fleet.env")
		if err := creds.SetEnvKey(path, "TAVILY_API_KEY", value); err != nil {
			t.Fatalf("SetEnvKey(%q): %v", value, err)
		}
		if _, err := Load(path); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := os.Getenv("TAVILY_API_KEY"); got != value {
			t.Errorf("server parsed %q, want %q (writer/loader round-trip)", got, value)
		}
		os.Unsetenv("TAVILY_API_KEY")
	}
}
