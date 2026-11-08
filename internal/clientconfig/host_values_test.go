package clientconfig

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// A connector env value interpolated from the host is registered with the
// log redactor whatever its variable is called (SESSION matches no
// secret-name rule), so a connector echoing it on stderr is logged redacted;
// a value the manifest writes literally is not registered (#2074 review).
func TestHostInterpolatedConnectorValuesAreRedactedLiterals(t *testing.T) {
	const hostValue = "sessHostOpaque-4Kq9Zt2W" // matches no shape pattern
	t.Setenv("SOME_HOST_VAR", hostValue)

	env := resolveEnvMap(map[string]string{
		"SESSION": "${SOME_HOST_VAR}",
		"REGION":  "us-east-1-literal",
	}, nil)
	if env["SESSION"] != hostValue {
		t.Fatalf("SESSION = %q", env["SESSION"])
	}

	var out bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	l := logging.NewStderrLogger(slog.String("connector", "acme"))
	_, _ = l.Write([]byte("session=" + hostValue + " region=us-east-1-literal\n"))
	_ = l.Close()

	got := out.String()
	if strings.Contains(got, hostValue) {
		t.Fatalf("host-sourced connector value leaked: %s", got)
	}
	if !strings.Contains(got, "us-east-1-literal") {
		t.Fatalf("a literal manifest value was registered as a secret: %s", got)
	}
}
