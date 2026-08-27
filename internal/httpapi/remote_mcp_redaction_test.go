package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// The control-plane error path is one of the redactors that sees a
// runtime-acquired credential (#1274): the add-time / rotate-time probe relays
// the VENDOR's failure text, which can echo the key we just sent. remotemcp
// registers those credentials as literals at acquisition, so the relayed text
// must go through the process-wide scrubber — the token below matches no shape
// pattern and carries no "key=" marker, so it is scrubbed only if literal
// redaction actually reaches this response body.
func TestRemoteMCPErrorScrubsRegisteredControlPlaneSecret(t *testing.T) {
	const echoed = "placeholder-vendor-credential-Qm7Vt2Lp"
	agentcore.RegisterSecretLiteral(echoed)

	rr := httptest.NewRecorder()
	(&Server{}).remoteMCPError(rr, fmt.Errorf("could not connect to the MCP server: upstream rejected credential %s (401)", echoed))

	body := rr.Body.String()
	if strings.Contains(body, echoed) {
		t.Errorf("credential echoed by the upstream survived into the response body: %q", body)
	}
	if !strings.Contains(body, "could not connect to the MCP server") {
		t.Errorf("scrubbing ate the actionable error text: %q", body)
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}
