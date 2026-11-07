// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeCosign writes a shell script standing in for cosign that records its
// argv and exits with rc.
func writeFakeCosign(t *testing.T, rc int) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "cosign")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho 'fake cosign says no' >&2\nexit " + map[bool]string{true: "0", false: "1"}[rc == 0] + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestCosignVerifyBlobPassesThePinnedIdentity(t *testing.T) {
	bin, argsFile := writeFakeCosign(t, 0)
	dir := t.TempDir()
	blob, bundle := filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "SHA256SUMS.sigstore.json")
	_ = os.WriteFile(blob, []byte("x"), 0o600)
	_ = os.WriteFile(bundle, []byte("{}"), 0o600)
	c := &Cosign{Bin: bin, Policy: Policy{Identity: SignerIdentity, Issuer: OIDCIssuer}}
	if err := c.VerifyBlob(context.Background(), blob, bundle); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(argsFile)
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{"verify-blob", "--bundle", bundle, "--certificate-identity", SignerIdentity, "--certificate-oidc-issuer", OIDCIssuer, blob}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("cosign argv\n got %q\nwant %q", args, want)
	}
	// Exact identity, never a regexp: a regexp is how a "fleet" pattern ends up
	// matching a fork.
	if strings.Contains(string(raw), "regexp") {
		t.Error("identity must be matched exactly")
	}
}

func TestCosignFailureIsAVerificationFailure(t *testing.T) {
	bin, _ := writeFakeCosign(t, 1)
	dir := t.TempDir()
	blob, bundle := filepath.Join(dir, "f"), filepath.Join(dir, "f.sigstore.json")
	_ = os.WriteFile(blob, []byte("x"), 0o600)
	_ = os.WriteFile(bundle, []byte("{}"), 0o600)
	c := &Cosign{Bin: bin, Policy: FleetPolicy()}
	err := c.VerifyBlob(context.Background(), blob, bundle)
	if err == nil || !strings.Contains(err.Error(), "FAILED") || !strings.Contains(err.Error(), "fake cosign says no") {
		t.Fatalf("want the cosign failure surfaced, got %v", err)
	}
	// A missing bundle never reaches cosign.
	if err := c.VerifyBlob(context.Background(), blob, filepath.Join(dir, "absent")); err == nil || !strings.Contains(err.Error(), "no signature") {
		t.Fatalf("missing bundle: %v", err)
	}
	// An empty policy is refused outright rather than handed to cosign.
	if err := (&Cosign{Bin: bin}).VerifyBlob(context.Background(), blob, bundle); err == nil {
		t.Fatal("an empty policy verified")
	}
}

func TestCosignVerifyImageNeedsADigest(t *testing.T) {
	bin, argsFile := writeFakeCosign(t, 0)
	c := &Cosign{Bin: bin, Policy: FleetPolicy()}
	if err := c.VerifyImage(context.Background(), SandboxImage+":latest"); err == nil {
		t.Fatal("a tag reference was verified; a tag can move after verification")
	}
	ref := SandboxImage + "@sha256:" + strings.Repeat("0", 64)
	if err := c.VerifyImage(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(raw), SignerIdentity) || !strings.HasSuffix(strings.TrimSpace(string(raw)), ref) {
		t.Fatalf("cosign verify argv: %q", raw)
	}
}

func TestEnsureCosignRefusesBytesThatAreNotThePin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\necho not cosign\n"))
	}))
	defer srv.Close()
	t.Setenv("FLEET_COSIGN_URL", srv.URL)
	cache := t.TempDir()
	_, err := EnsureCosign(context.Background(), NewDownloader(), cache, "amd64")
	if err == nil || !strings.Contains(err.Error(), "refusing to run it") {
		t.Fatalf("want a digest refusal, got %v", err)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("rejected download left files behind: %v", entries)
	}
	// A planted file at the cache path is not trusted either: it is re-fetched
	// (and here refused again) rather than executed.
	planted := filepath.Join(cache, "cosign-"+PinnedCosign().Version+"-linux-amd64")
	_ = os.WriteFile(planted, []byte("planted"), 0o755)
	if _, err := EnsureCosign(context.Background(), NewDownloader(), cache, "amd64"); err == nil {
		t.Fatal("a planted cache file was used")
	}
	if _, err := EnsureCosign(context.Background(), NewDownloader(), cache, "s390x"); err == nil {
		t.Fatal("an unpinned arch was accepted")
	}
}

// TestRecordedSigstoreVector runs the REAL cosign, offline, against a recorded
// keyless signature: Sigstore's own cosign v3.1.4 cosign_checksums.txt, its
// bundle, and the public-good trusted root at the time of recording
// (testdata/sigstore-vector). It proves the Cosign verifier — the exact argv
// fleet runs — accepts a genuine keyless bundle for the right identity and
// rejects the wrong identity, a tampered file and a bundle for another file.
//
// It needs the pinned cosign binary, which is not vendored (145 MB): set
// FLEET_TEST_COSIGN to its path. The test checks the binary IS the pin before
// trusting the result.
func TestRecordedSigstoreVector(t *testing.T) {
	bin := os.Getenv("FLEET_TEST_COSIGN")
	if bin == "" {
		t.Skip("set FLEET_TEST_COSIGN to the pinned cosign binary to run the recorded Sigstore vector")
	}
	if got, _ := FileSHA256(bin); got != PinnedCosign().SHA256["linux/"+runtime.GOARCH] {
		t.Fatalf("FLEET_TEST_COSIGN is not the pinned cosign (sha256 %s)", got)
	}
	vec, _ := filepath.Abs("testdata/sigstore-vector")
	blob := filepath.Join(vec, "cosign_checksums.txt")
	bundle := blob + ".sigstore.json"
	root := filepath.Join(vec, "trusted_root.json")
	// Sigstore signs its releases from Google Cloud Build.
	sigstore := Policy{Identity: "keyless@projectsigstore.iam.gserviceaccount.com", Issuer: "https://accounts.google.com", TrustedRoot: root}
	// Offline: no TUF fetch, no network at all.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	ctx := context.Background()

	ok := &Cosign{Bin: bin, Policy: sigstore, TUFRoot: t.TempDir()}
	if err := ok.VerifyBlob(ctx, blob, bundle); err != nil {
		t.Fatalf("genuine signature by the right identity rejected: %v", err)
	}

	fleet := &Cosign{Bin: bin, Policy: Policy{Identity: SignerIdentity, Issuer: OIDCIssuer, TrustedRoot: root}, TUFRoot: t.TempDir()}
	if err := fleet.VerifyBlob(ctx, blob, bundle); err == nil {
		t.Fatal("a genuine signature by ANOTHER identity was accepted under the fleet policy")
	}

	wrongIssuer := &Cosign{Bin: bin, Policy: Policy{Identity: sigstore.Identity, Issuer: OIDCIssuer, TrustedRoot: root}, TUFRoot: t.TempDir()}
	if err := wrongIssuer.VerifyBlob(ctx, blob, bundle); err == nil {
		t.Fatal("the right identity from the wrong issuer was accepted")
	}

	tampered := filepath.Join(t.TempDir(), "cosign_checksums.txt")
	raw, _ := os.ReadFile(blob)
	_ = os.WriteFile(tampered, append(raw, '\n'), 0o600)
	if err := ok.VerifyBlob(ctx, tampered, bundle); err == nil {
		t.Fatal("a tampered file was accepted")
	}

	other := filepath.Join(t.TempDir(), "other")
	_ = os.WriteFile(other, []byte("not the signed file"), 0o600)
	if err := ok.VerifyBlob(ctx, other, bundle); err == nil {
		t.Fatal("a bundle for another file was accepted")
	}
}
