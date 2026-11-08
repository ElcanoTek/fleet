package agent

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// A connector whose first initialize is refused with a body echoing the
// opaque key it was sent has that failure logged at boot — before any lazy
// redactor registration ran. BuildMCPClient registers the connectors' header
// credentials first, so the logged error is scrubbed by value (#2074 review).
func TestConnectorInitFailureEchoingItsHeaderIsLoggedRedacted(t *testing.T) {
	const opaque = "acmeOpaqueHdrVal7Qw9Zr3" // matches no shape pattern
	const bearer = "acmeOpaqueBearer5Tk8Pm2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("rejected key " + r.Header.Get("X-Api-Key") + " and " + strings.TrimPrefix(r.Header.Get("Authorization"), "Token ")))
	}))
	defer srv.Close()

	var out syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	BuildMCPClient(map[string]MCPServerSpec{
		"acme": {Enabled: true, URL: srv.URL, Headers: map[string]string{"X-Api-Key": opaque, "Authorization": "Token " + bearer, "Accept": "application/json"}},
	}, nil, nil)

	got := string(out.Bytes())
	if !strings.Contains(got, "MCP server failed to connect") {
		t.Fatalf("no connect-failure line logged:\n%s", got)
	}
	if !strings.Contains(got, "rejected key") {
		t.Fatalf("the failure did not carry the echoed body, so the test proves nothing:\n%s", got)
	}
	for _, leak := range []string{opaque, bearer} {
		if strings.Contains(got, leak) {
			t.Fatalf("connector credential %q logged:\n%s", leak, got)
		}
	}
	if strings.Contains(got, "[REDACTED]json") {
		t.Fatalf("a non-secret header value was registered as a literal:\n%s", got)
	}
}
