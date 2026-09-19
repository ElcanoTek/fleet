package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// Inspect the collected overlap names without relying on the public/loggable
// error string. Exhaustive alias-refusal tests assert the refused names here;
// boot logs only see a count.
func overlapNames(t *testing.T, bundle *clientconfig.Bundle) string {
	t.Helper()
	return strings.Join(connectorParentEnvOverlap(bundle), ", ")
}

func requireOverlap(t *testing.T, bundle *clientconfig.Bundle, wantName string) error {
	t.Helper()
	err := validateConnectorParentEnvSeparation(bundle)
	if err == nil {
		t.Fatalf("expected overlap refusal for %s", wantName)
	}
	if !strings.Contains(overlapNames(t, bundle), wantName) {
		t.Fatalf("overlap names = %q, want %s", overlapNames(t, bundle), wantName)
	}
	if strings.Contains(err.Error(), wantName) {
		t.Fatalf("loggable error echoed %s: %v", wantName, err)
	}
	return err
}

func TestOverlapErrorDoesNotEchoRawConfiguration(t *testing.T) {
	const pasted = "FAKE_PASTED_CREDENTIAL"
	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: demo
    type: stdio
    command: /bin/true
    always: true
    env:
      TOKEN: "${`+pasted+`}"
providers:
  - name: models
    type: openai
    api_key_env: `+pasted+`
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	overlapErr := requireOverlap(t, bundle, pasted)
	if !strings.Contains(overlapErr.Error(), "1 fields") {
		t.Fatalf("missing actionable diagnostic: %v", overlapErr)
	}
	wrapped := fmt.Errorf("start production MCP broker: %w", overlapErr)
	if strings.Contains(wrapped.Error(), pasted) {
		t.Fatalf("wrapped boot error echoed raw field: %v", wrapped)
	}
}
