package agentcore

import (
	"strings"
	"testing"
)

// TestSavedToolCallInputKeepsVaultReferences: a tool call is saved (and so
// replayed to the model on later turns) through toolRedactor. A credential
// reference survives it, so the model still knows which credential its own
// earlier call used; a literal credential in the same position does not.
func TestSavedToolCallInputKeepsVaultReferences(t *testing.T) {
	in := `{"action":"probe_browser","url":"https://a.example.com/","password_ref":"$secret:password","secret_ref":"$secret:api_token"}`
	got := toolRedactor().Redact(in)
	for _, keep := range []string{`"password_ref":"$secret:password"`, `"secret_ref":"$secret:api_token"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("saved input = %s, want it to keep %s", got, keep)
		}
	}
	literal := toolRedactor().Redact(`{"action":"probe_browser","password_ref":"literal-password-1928"}`)
	if strings.Contains(literal, "literal-password-1928") {
		t.Errorf("saved input = %s, want the literal password redacted", literal)
	}
}
