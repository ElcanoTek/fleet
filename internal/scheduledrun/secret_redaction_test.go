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

// TestRedactSecrets_MarkerlessBoundary documents (and locks) the CURRENT
// coverage boundary of the redaction regex: it only fires after an
// api_key/token/secret/password/authorization marker followed by a
// [:\s=] separator. A secret embedded in markerless JSON (the marker is a
// quoted key, so the separator is `"` which the regex does not accept) passes
// through unredacted. This is a known limitation, asserted here so a future
// tightening of secretPattern is a deliberate, test-visible change rather than a
// silent behavior shift.
func TestRedactSecrets_MarkerlessBoundary(t *testing.T) {
	markerless := `{"some_value":"` + sentinel + `"}`
	if got := agentcore.RedactSecrets(markerless); !strings.Contains(got, sentinel) {
		t.Fatalf("redaction unexpectedly scrubbed a markerless value (regex was tightened?): %q -> %q", markerless, got)
	}
	// And the marker+separator form IS scrubbed, for contrast.
	marked := "authorization=Bearer " + sentinel
	if got := agentcore.RedactSecrets(marked); strings.Contains(got, sentinel) {
		t.Fatalf("marker+separator secret was not redacted: %q -> %q", marked, got)
	}
}
