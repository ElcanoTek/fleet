package scheduledrun

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// sentinel is a fake, marker-prefixed secret value used only in these tests. It
// is intentionally long and free of the value-class delimiters the redaction
// regex stops on (\s "',}{), so it is exactly the shape the scrubber targets.
const sentinel = "SENTINELVALUEabc123def456"

// TestConvertLogSession_RedactsSecretsEndToEnd pins the project's headline
// "...or logs" invariant end-to-end: a known secret echoed into message content,
// reasoning, and tool-call arguments must not survive into the persisted
// LogSession. It drives the real convertLogSession persist path (previously
// exercised by zero tests) rather than calling the regex in isolation.
func TestConvertLogSession_RedactsSecretsEndToEnd(t *testing.T) {
	ls := agent.NewLogSession()
	ls.AddMessage(roleAssistant, "the connector key is api_key="+sentinel+" — use it", nil, nil)
	ls.AddMessageWithMetadata(
		roleAssistant, "calling the tool", nil, nil, nil,
		[]agentcore.LogToolCall{{ID: "tc-1", Name: "mcp_demo_do", Arguments: "secret=" + sentinel}},
		nil, "internal note: token: "+sentinel,
	)

	out := convertLogSession(nil, ls)
	if out == nil {
		t.Fatal("convertLogSession returned nil")
	}
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal persisted log: %v", err)
	}
	persisted := string(blob)

	if strings.Contains(persisted, sentinel) {
		t.Errorf("secret sentinel survived into the persisted log session:\n%s", persisted)
	}
	if !strings.Contains(persisted, "[REDACTED]") {
		t.Errorf("expected [REDACTED] markers in the persisted log; got:\n%s", persisted)
	}
}

const roleAssistant = "assistant"

// TestRedactSecrets_ClosedGaps locks the WIDENED coverage of the redactor (#307,
// internal/redact): the JSON-quoted marker form and vendor key prefixes that the
// old marker-only regex let through are now scrubbed, while a genuinely
// markerless, non-secret value is deliberately left alone (we scrub known shapes
// and registered literals, not every long string).
func TestRedactSecrets_ClosedGaps(t *testing.T) {
	// Now-closed gap 1: a marker inside JSON ({"api_key":"..."}) — the separator
	// is a quote, which the old regex rejected.
	jsonMarker := `{"api_key":"` + sentinel + `"}`
	if got := agentcore.RedactSecrets(jsonMarker); strings.Contains(got, sentinel) {
		t.Errorf("JSON-marker secret survived: %q -> %q", jsonMarker, got)
	}

	// Now-closed gap 2: vendor key prefixes are scrubbed by shape, with no marker.
	vendor := "leaked sk-or-v1-0123456789abcdef0123456789abcdef in output"
	if got := agentcore.RedactSecrets(vendor); strings.Contains(got, "sk-or-v1-0123456789abcdef0123456789abcdef") {
		t.Errorf("vendor-prefix secret survived: %q -> %q", vendor, got)
	}

	// The marker+separator+Bearer form is still scrubbed.
	marked := "authorization=Bearer " + sentinel
	if got := agentcore.RedactSecrets(marked); strings.Contains(got, sentinel) {
		t.Errorf("marker+Bearer secret was not redacted: %q -> %q", marked, got)
	}

	// Deliberate boundary: a markerless, non-secret-shaped value is NOT scrubbed —
	// the redactor targets known shapes + registered literals, not arbitrary text.
	benign := `{"some_value":"` + sentinel + `"}`
	if got := agentcore.RedactSecrets(benign); !strings.Contains(got, sentinel) {
		t.Errorf("a markerless non-secret value was over-redacted: %q -> %q", benign, got)
	}
}
