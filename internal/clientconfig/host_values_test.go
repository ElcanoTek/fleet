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

// A value assembled from host fragments each too short to register alone
// (two 6-byte pieces) is registered whole (#2074 review).
func TestAssembledHostValueIsRegisteredWhole(t *testing.T) {
	t.Setenv("AUTH_A", "Qw7Zp2")
	t.Setenv("AUTH_B", "Lm9Xc4")
	env := resolveEnvMap(map[string]string{"AUTH": "${AUTH_A}${AUTH_B}"}, nil)
	if env["AUTH"] != "Qw7Zp2Lm9Xc4" {
		t.Fatalf("AUTH = %q", env["AUTH"])
	}
	if got := logging.Redact("connector sent Qw7Zp2Lm9Xc4"); strings.Contains(got, "Qw7Zp2Lm9Xc4") {
		t.Fatalf("assembled host value not registered: %q", got)
	}
}

// A path or path list interpolated from the host (CUTLASS_ALLOWED_DIRS built
// from ${CUTLASS_ALLOWED_DIRS}:${FLEET_WORKSPACE_ROOT}) is configuration, not
// a credential: it is not registered, so a log line naming a file under it
// stays readable (#2074 review), while a credential interpolated beside it
// still is.
func TestInterpolatedHostPathsAreNotRegistered(t *testing.T) {
	t.Setenv("CUTLASS_ALLOWED_DIRS", "/srv/dropsHostPath77")
	t.Setenv("ACME_EXTRA_DIR", "/var/lib/fleetWorkspace77")
	t.Setenv("ACME_SESSION", "acmeSessionOpaque-Qw81Zr")
	env := resolveEnvMap(map[string]string{
		"CUTLASS_ALLOWED_DIRS": "${CUTLASS_ALLOWED_DIRS}:${ACME_EXTRA_DIR}",
		"SESSION":              "${ACME_SESSION}",
	}, nil)
	if env["CUTLASS_ALLOWED_DIRS"] != "/srv/dropsHostPath77:/var/lib/fleetWorkspace77" {
		t.Fatalf("CUTLASS_ALLOWED_DIRS = %q", env["CUTLASS_ALLOWED_DIRS"])
	}
	got := logging.Redact("wrote /srv/dropsHostPath77/report.csv under /var/lib/fleetWorkspace77/run-1; session acmeSessionOpaque-Qw81Zr")
	if !strings.Contains(got, "/srv/dropsHostPath77/report.csv") || !strings.Contains(got, "/var/lib/fleetWorkspace77/run-1") {
		t.Fatalf("an interpolated host path was registered as a secret: %q", got)
	}
	if strings.Contains(got, "acmeSessionOpaque-Qw81Zr") {
		t.Fatalf("the interpolated credential was not registered: %q", got)
	}
}
