package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// TestConnectorStudioRefusesRedactedArguments: any connector_studio argument
// that carries the redactor's placeholder (a probe_http header, a custom
// tool's body, an API key prefix) is refused before anything is sent.
func TestConnectorStudioRefusesRedactedArguments(t *testing.T) {
	t.Parallel()
	h := (&Server{}).newConnectorStudioHandler(nil, "alice@example.com")
	for _, p := range []tools.ConnectorStudioParams{
		{Action: "probe_http", URL: "https://a.example.com/", Headers: map[string]string{"Authorization": "Bearer [REDACTED]"}},
		{Action: "create_custom_connector", Name: "x", URL: "https://a.example.com/", CustomTools: []tools.ConnectorStudioToolSpec{{Name: "t", URL: "https://a.example.com/", Body: `{"key":"[REDACTED]"}`}}},
		{Action: "connect_mcp", URL: "https://a.example.com/mcp", APIKeyPrefix: "[REDACTED] "},
	} {
		if _, err := h(context.Background(), p); !errors.Is(err, tools.ErrRedactionMarkerArgument) {
			t.Errorf("%s: err = %v, want ErrRedactionMarkerArgument", p.Action, err)
		}
	}
}
