package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
)

// TestVaultedSecretErrorsNameTheFieldThroughTheRedactor: the errors about a
// vaulted secret reach the model through the tool-output redactor, which
// masks "secret:<eight or more characters>". They name the form field in
// words, so the model can still tell which credential the error is about.
func TestVaultedSecretErrorsNameTheFieldThroughTheRedactor(t *testing.T) {
	t.Parallel()
	const user = "admin@example.com"
	chatSt := newFakeChatStore()
	conv, _ := chatSt.CreateConversation(context.Background(), user, "Build Connector", "default", "", false)
	s := &Server{
		store:     chatSt,
		remoteMCP: remotemcp.NewService(newMemRemoteMCPStore(), remotemcp.Config{PublicBaseURL: "https://fleet.example.com"}),
	}
	s.uiSecrets.put(user, conv.ID, "password", "pw-value-1928")
	s.uiSecrets.allowOrigin(user, conv.ID, "https://a.example.com")

	// The reference form is what the redactor masks, which is why the
	// messages do not use it.
	if got := agentcore.RedactSecrets(`vaulted secret "$secret:password" is not authorized`); !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("the redactor no longer masks $secret:password (%q); this test's premise changed", got)
	}

	cases := []struct {
		name, ref, target, want string
	}{
		{"not authorized for the host", "$secret:password", "https://b.example.com/login",
			`the vaulted secret from the "password" field is not authorized for host "https://b.example.com"`},
		{"never submitted or expired", "$secret:client_secret_value", "https://a.example.com/login",
			`no vaulted secret from the "client_secret_value" field: it was never submitted or has expired`},
		{"not an HTTPS target", "$secret:password", "http://a.example.com/login",
			`the vaulted secret from the "password" field needs a valid HTTPS target URL`},
		{"a reference without a field name", "$secret:", "https://a.example.com/login",
			`no vaulted secret from a reference that names no field`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.resolveSecretForURL(context.Background(), user, conv.ID, tc.ref, tc.target)
			if err == nil {
				t.Fatal("resolveSecretForURL succeeded, want an error")
			}
			seen := agentcore.RedactSecrets(err.Error())
			if !strings.Contains(seen, tc.want) {
				t.Errorf("the model sees %q, want it to contain %q", seen, tc.want)
			}
			if strings.Contains(seen, "[REDACTED]") {
				t.Errorf("the model sees a masked message: %q", seen)
			}
		})
	}
}
