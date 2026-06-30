package admincli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/redact"
)

// readTarGz returns a map of member name → body for a gzipped tar at path.
func readTarGz(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read member %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(body)
	}
	return out
}

// TestDiagnoseProducesBundleWithNoSecretValues is the core acceptance test: the
// collector writes a tarball that contains every section file, lists secret env
// var NAMES but never their VALUES, and scrubs a secret-shaped token that appears
// in section text.
func TestDiagnoseProducesBundleWithNoSecretValues(t *testing.T) {
	// An obviously-fake secret value that MUST NOT appear in the bundle. Built at
	// runtime (not a source literal) so it is clearly a placeholder and never trips
	// the repo's gitleaks gate, while still being long enough (>= minLiteralLen) to
	// exercise the literal-by-env-value redaction path — the core guarantee. The
	// redactor registers WHATEVER value the secret-named env var holds, regardless
	// of shape, so a generic placeholder validates the same path a real key would.
	secretValue := "PLACEHOLDER-NOT-A-REAL-" + strings.Repeat("X", 32)
	t.Setenv("OPENROUTER_API_KEY", secretValue)
	t.Setenv("FLEET_SOME_TOKEN", "tok-"+secretValue) // arbitrary secret-named var

	dir := t.TempDir()
	bundleDir := filepath.Join("..", "..", "config", "default") // the in-repo generic bundle
	dc := &diagnoseCollector{
		bundleDir:   bundleDir,
		service:     "fleet",
		skipSandbox: true, // don't depend on podman in unit tests
		redactor: func() *redact.Redactor {
			r := redact.NewRedactor(nil)
			r.RegisterEnvLiterals(os.Environ())
			return r
		}(),
	}
	out := filepath.Join(dir, "bundle.tar.gz")
	if err := dc.write(out); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	members := readTarGz(t, out)
	for _, want := range []string{"status.txt", "config.txt", "db.txt", "sandbox.txt"} {
		if _, ok := members[want]; !ok {
			t.Errorf("bundle missing section %q (have %v)", want, keysOf(members))
		}
	}

	// No section may contain the raw secret value (the central guarantee).
	for name, body := range members {
		if strings.Contains(body, secretValue) {
			t.Errorf("section %q leaked the secret value", name)
		}
	}

	// config.txt lists the env var NAMES (so the operator sees what is set) but
	// never the values.
	cfg := members["config.txt"]
	for _, name := range []string{"OPENROUTER_API_KEY", "FLEET_SOME_TOKEN"} {
		if !strings.Contains(cfg, name) {
			t.Errorf("config.txt should list env var name %q", name)
		}
	}
	// The generic bundle loads, so its app name should be present.
	if !strings.Contains(cfg, "app_name:") {
		t.Errorf("config.txt should summarize the bundle (app_name); got:\n%s", cfg)
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestCaptureHealthReusesStatusChecks confirms the status section reuses the real
// status report format (the ✓/✗ header) rather than a separate implementation.
func TestCaptureHealthReusesStatusChecks(t *testing.T) {
	bundleDir := filepath.Join("..", "..", "config", "default")
	// No DB DSNs set and sandbox skipped: the checks still run and produce a
	// report; we only assert on its SHAPE, not health, so it is hermetic.
	got := captureHealth(context.Background(), "", "", bundleDir, "fleet", true)
	if !strings.Contains(got, "deployment health") {
		t.Errorf("captureHealth output missing the status header; got:\n%s", got)
	}
	if !strings.Contains(got, "client bundle") {
		t.Errorf("captureHealth should include the client bundle check; got:\n%s", got)
	}
}

// TestDiagnoseSandboxRefResolution verifies the sandbox section reports the env
// override ref and notes when podman inspection is skipped.
func TestDiagnoseSandboxRefResolution(t *testing.T) {
	t.Setenv("FLEET_SANDBOX_IMAGE", "localhost/fleet-sandbox:diagnose-test")
	dc := &diagnoseCollector{bundleDir: filepath.Join("..", "..", "config", "default"), skipSandbox: true}
	body := dc.collectSandbox(context.Background())
	if !strings.Contains(body, "localhost/fleet-sandbox:diagnose-test") {
		t.Errorf("sandbox section should echo the resolved ref; got:\n%s", body)
	}
	if !strings.Contains(body, "--no-sandbox") {
		t.Errorf("sandbox section should note the skip; got:\n%s", body)
	}
}
