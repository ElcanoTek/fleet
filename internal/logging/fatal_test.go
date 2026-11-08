package logging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// runFatal calls Fatal with os.Stderr captured and exit stubbed out, and
// returns what reached stderr and the exit code.
func runFatal(t *testing.T, msg string, args ...any) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevStderr, prevExit := os.Stderr, exitFunc
	code := -1
	os.Stderr = w
	exitFunc = func(c int) { code = c }
	defer func() { os.Stderr, exitFunc = prevStderr, prevExit }()
	Fatal(msg, args...)
	os.Stderr = prevStderr
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out), code
}

// A fatal startup error before Configure — a config or bundle error quoting
// an interpolated secret — goes out through the emergency handler, redacted
// by the process's own secret-named env values, buffered startup lines
// included (#2074 review: the emergency path never seeded the redactor).
func TestFatalBeforeConfigureRedactsEnvSecrets(t *testing.T) {
	restoreLogging(t)
	configured.Store(false)
	const opaque = "fatalOpaqueEnvSecret7Qw2" // matches no shape pattern
	t.Setenv("ACME_SESSION_TOKEN", opaque)

	BufferStartup()
	slog.Info("loading bundle", "detail", "header value "+opaque)
	out, code := runFatal(t, "load client config bundle", "err", errors.New("connector acme: bad header "+opaque))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(out, opaque) {
		t.Fatalf("the env secret reached stderr:\n%s", out)
	}
	if !strings.Contains(out, `"msg":"load client config bundle"`) || !strings.Contains(out, `"msg":"loading bundle"`) {
		t.Fatalf("fatal or buffered line missing:\n%s", out)
	}
}

// Without a startup buffer (a CLI verb) the default handler before Configure
// is Go's own, which redacts nothing: Fatal must not use it.
func TestFatalWithoutBufferUsesTheRedactingHandler(t *testing.T) {
	restoreLogging(t)
	configured.Store(false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // stands in for Go's default
	const opaque = "fatalOpaqueEnvSecret9Lk4"
	t.Setenv("ACME_API_KEY", opaque)
	out, _ := runFatal(t, "bad config", "err", "quoted "+opaque)
	if strings.Contains(out, opaque) || !strings.Contains(out, `"msg":"bad config"`) {
		t.Fatalf("stderr = %q", out)
	}
}

// A secret the configuration registry marks Secret is registered whatever
// its name: DATABASE_URL names no credential, and an INVALID one (a bad
// port, so url.Parse refuses it) is exactly what a fatal config error
// quotes. Neither the DSN nor its password alone may reach the fatal line
// (#2074 review).
func TestFatalRedactsAnInvalidDatabaseURLPassword(t *testing.T) {
	restoreLogging(t)
	configured.Store(false)
	const password = "dbOpaquePw4Rt8Yh" // matches no shape pattern; gitleaks:allow — a FAKE test fixture, not a credential
	dsn := "postgres://fleet:" + password + "@db.internal:notaport/fleet?sslmode=disable"
	t.Setenv("DATABASE_URL", dsn)
	BufferStartup()
	out, code := runFatal(t, "fleet: failed", "err",
		errors.New("load config: invalid DATABASE_URL "+dsn+": password "+password+" rejected"))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(out, password) || !strings.Contains(out, `"msg":"fleet: failed"`) {
		t.Fatalf("the DATABASE_URL password reached stderr:\n%s", out)
	}
}

// The credential parts of a DSN, in either form, as written and decoded.
func TestDSNCredentials(t *testing.T) {
	got := strings.Join(dsnCredentials("postgres://u:p%40ss@h:bad/db?sslpassword=k3y&x=1"), "|")
	for _, want := range []string{"u:p%40ss", "p%40ss", "p@ss", "k3y"} {
		if !strings.Contains("|"+got+"|", "|"+want+"|") {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	got = strings.Join(dsnCredentials(`host=db password='a b\'c' user=x`), "|")
	if got != "a b'c" {
		t.Fatalf("keyword DSN: %q", got)
	}
}
