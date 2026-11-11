package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// TestRedactedPlaceholderIsNotACredential: the model may copy "[REDACTED]"
// out of its own earlier tool call, where the redactor replaced a secret.
// Used as a credential it would be typed into the site or sent as the key,
// so it is refused, with a reason the model can still read after redaction.
func TestRedactedPlaceholderIsNotACredential(t *testing.T) {
	t.Parallel()
	s := &Server{}
	for _, ref := range []string{"[REDACTED]", "$secret:[REDACTED]", "prefix-[REDACTED PRIVATE KEY]"} {
		_, err := s.resolveSecretForURL(context.Background(), "alice@example.com", "c1", ref, "https://a.example.com/login")
		if err == nil {
			t.Fatalf("resolveSecretForURL(%q) succeeded, want it refused", ref)
		}
		seen := agentcore.RedactSecrets(err.Error())
		if !strings.Contains(seen, "the placeholder the conversation shows in place of a secret") || !strings.Contains(seen, "show_ui") {
			t.Errorf("the model sees %q for %q", seen, ref)
		}
	}
}
