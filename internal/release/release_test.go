// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// fakeELF is a minimal, valid ELF64 header for machine m — enough for
// debug/elf to read the architecture, which is all checkELFArch looks at.
func fakeELF(m elf.Machine, payload string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT), 0})
	b.Write(make([]byte, 8))
	hdr := struct {
		Type, Machine                                        uint16
		Version                                              uint32
		Entry, Phoff, Shoff                                  uint64
		Flags                                                uint32
		Ehsize, Phentsize, Phnum, Shentsize, Shnum, Shstrndx uint16
	}{Type: uint16(elf.ET_EXEC), Machine: uint16(m), Version: uint32(elf.EV_CURRENT), Ehsize: 64}
	_ = binary.Write(&b, binary.LittleEndian, hdr)
	b.WriteString(payload)
	return b.Bytes()
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// fakeSig is the stand-in signature format the fakeVerifier checks: it binds
// the signed bytes' digest to a signer identity, which is exactly what a
// Sigstore bundle does (minus the cryptography, which the recorded-vector test
// below exercises with the real cosign).
func fakeSig(content []byte, identity string) []byte {
	return []byte("sig:" + sha(content) + ":" + identity)
}

type fakeVerifier struct {
	identity string
	mu       sync.Mutex
	calls    []string
	images   map[string]error
}

func (f *fakeVerifier) VerifyBlob(_ context.Context, path, bundle string) error {
	f.mu.Lock()
	f.calls = append(f.calls, filepath.Base(path))
	f.mu.Unlock()
	sig, err := os.ReadFile(bundle)
	if err != nil {
		return fmt.Errorf("no bundle: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	parts := strings.SplitN(string(sig), ":", 3)
	if len(parts) != 3 || parts[0] != "sig" {
		return errors.New("malformed bundle")
	}
	if parts[1] != sha(content) {
		return errors.New("signature does not match the file")
	}
	if parts[2] != f.identity {
		return fmt.Errorf("expected identity %q, got %q", f.identity, parts[2])
	}
	return nil
}

func (f *fakeVerifier) VerifyImage(_ context.Context, ref string) error {
	if err, ok := f.images[ref]; ok {
		return err
	}
	return errors.New("no signature for image")
}

const testTag = "v2026.11.07.3"

var sandboxRef = SandboxImage + "@sha256:" + strings.Repeat("ab", 32)

// relFixture is one release's files as a server would publish them.
type relFixture map[string][]byte

func newRelease(t *testing.T, tag string, signer string) relFixture {
	t.Helper()
	files := relFixture{}
	files[BinaryAsset("amd64")] = fakeELF(elf.EM_X86_64, "fleet amd64 "+tag)
	files[BinaryAsset("arm64")] = fakeELF(elf.EM_AARCH64, "fleet arm64 "+tag)
	man, _ := json.Marshal(Manifest{Schema: 1, Tag: tag, Commit: strings.Repeat("c", 40),
		Images: map[string]string{SandboxImage: sandboxRef}})
	files[ManifestFile] = man
	files.reseal(signer)
	return files
}

// reseal rewrites SHA256SUMS over the current files and re-signs everything
// with signer.
func (r relFixture) reseal(signer string) {
	var sums strings.Builder
	for _, n := range []string{BinaryAsset("amd64"), BinaryAsset("arm64"), ManifestFile} {
		fmt.Fprintf(&sums, "%s  %s\n", sha(r[n]), n)
		r[n+BundleSuffix] = fakeSig(r[n], signer)
	}
	r[SumsFile] = []byte(sums.String())
	r[SumsFile+BundleSuffix] = fakeSig(r[SumsFile], signer)
}

func serve(t *testing.T, r relFixture) string {
	const tag = testTag
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/"+tag+"/")
		b, ok := r[name]
		if !ok || name == req.URL.Path {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func mustTag(t *testing.T, s string) Tag {
	t.Helper()
	tag, err := ParseTag(s)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func fetch(t *testing.T, base string, o FetchOptions) (*Fetched, *fakeVerifier, error) {
	t.Helper()
	v := &fakeVerifier{identity: SignerIdentity}
	if o.Arch == "" {
		o.Arch = "amd64"
	}
	if o.Tag == (Tag{}) {
		o.Tag = mustTag(t, testTag)
	}
	o.Dir = t.TempDir()
	o.BaseURL = base
	got, err := Fetch(context.Background(), NewDownloader(), v, o)
	return got, v, err
}

// ── Fetch: the verifier must reject every class of bad release ─────────────

func TestFetchVerifiesAGoodRelease(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		r := newRelease(t, testTag, SignerIdentity)
		got, v, err := fetch(t, serve(t, r), FetchOptions{Arch: arch, Current: "2026.11.07.2"})
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		b, _ := os.ReadFile(got.Binary)
		if !bytes.Equal(b, r[BinaryAsset(arch)]) {
			t.Fatalf("%s: wrong bytes installed", arch)
		}
		if fi, _ := os.Stat(got.Binary); fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s: verified binary was not made executable", arch)
		}
		if want := []string{SumsFile, BinaryAsset(arch)}; strings.Join(v.calls, ",") != strings.Join(want, ",") {
			t.Errorf("%s: signatures checked %v, want %v", arch, v.calls, want)
		}
		if ref, _ := got.Manifest.ImageRef(SandboxImage); ref != sandboxRef {
			t.Errorf("sandbox ref %q", ref)
		}
	}
}

func TestFetchRejectsAWrongChecksum(t *testing.T) {
	r := newRelease(t, testTag, SignerIdentity)
	// The binary is swapped after SHA256SUMS was signed; its own bundle is
	// re-signed so only the checksum can catch it.
	r[BinaryAsset("amd64")] = fakeELF(elf.EM_X86_64, "a different binary")
	r[BinaryAsset("amd64")+BundleSuffix] = fakeSig(r[BinaryAsset("amd64")], SignerIdentity)
	got, _, err := fetch(t, serve(t, r), FetchOptions{})
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("want a checksum refusal, got %v", err)
	}
	if got != nil {
		t.Fatal("a refused release returned a binary")
	}
}

func TestFetchRejectsAWrongSignature(t *testing.T) {
	r := newRelease(t, testTag, SignerIdentity)
	// SHA256SUMS edited after signing (the attacker re-points a checksum at
	// their binary): the bundle no longer covers the file.
	r[BinaryAsset("amd64")] = fakeELF(elf.EM_X86_64, "evil")
	r[SumsFile] = []byte(strings.Replace(string(r[SumsFile]), sha(newRelease(t, testTag, SignerIdentity)[BinaryAsset("amd64")]), sha(r[BinaryAsset("amd64")]), 1))
	_, v, err := fetch(t, serve(t, r), FetchOptions{})
	if err == nil || !strings.Contains(err.Error(), "signature does not match") {
		t.Fatalf("want a signature refusal, got %v", err)
	}
	if len(v.calls) != 1 {
		t.Errorf("verification must stop at SHA256SUMS; checked %v", v.calls)
	}
}

func TestFetchRejectsAWrongIdentity(t *testing.T) {
	for _, signer := range []string{
		"https://github.com/ElcanoTek/fleet/.github/workflows/ci.yml@refs/heads/main",         // another workflow
		"https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/feature", // another branch
		"https://github.com/attacker/fleet/.github/workflows/release.yml@refs/heads/main",     // a fork
		"someone@example.com",
	} {
		r := newRelease(t, testTag, signer)
		_, _, err := fetch(t, serve(t, r), FetchOptions{})
		if err == nil || !strings.Contains(err.Error(), "expected identity") {
			t.Errorf("signer %q: want an identity refusal, got %v", signer, err)
		}
	}
}

func TestFetchRejectsAMissingSignature(t *testing.T) {
	for _, missing := range []string{SumsFile + BundleSuffix, BinaryAsset("amd64") + BundleSuffix} {
		r := newRelease(t, testTag, SignerIdentity)
		delete(r, missing)
		got, _, err := fetch(t, serve(t, r), FetchOptions{})
		if err == nil || !strings.Contains(err.Error(), "no signature") {
			t.Errorf("without %s: want a missing-signature refusal, got %v", missing, err)
		}
		if got != nil {
			t.Errorf("without %s: an unsigned release returned a binary", missing)
		}
	}
}

func TestFetchRejectsAWrongArchitecture(t *testing.T) {
	r := newRelease(t, testTag, SignerIdentity)
	// A mislabelled asset: the arm64 binary published under the amd64 name,
	// correctly checksummed and signed. Only the ELF check can catch it.
	r[BinaryAsset("amd64")] = r[BinaryAsset("arm64")]
	r.reseal(SignerIdentity)
	_, _, err := fetch(t, serve(t, r), FetchOptions{Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "built for EM_AARCH64") {
		t.Fatalf("want an architecture refusal, got %v", err)
	}

	_, _, err = fetch(t, serve(t, newRelease(t, testTag, SignerIdentity)), FetchOptions{Arch: "riscv64"})
	if err == nil || !strings.Contains(err.Error(), "linux/riscv64") {
		t.Fatalf("want an unsupported-arch refusal, got %v", err)
	}
}

func TestFetchRefusesADowngrade(t *testing.T) {
	r := newRelease(t, testTag, SignerIdentity)
	base := serve(t, r)
	_, v, err := fetch(t, base, FetchOptions{Current: "2026.11.08.1+2.gabcdef012345"})
	if !errors.Is(err, ErrDowngrade) {
		t.Fatalf("want ErrDowngrade, got %v", err)
	}
	if len(v.calls) != 0 {
		t.Error("the downgrade guard must run before anything is downloaded")
	}
	if _, _, err := fetch(t, base, FetchOptions{Current: "2026.11.08.1", AllowDowngrade: true}); err != nil {
		t.Fatalf("--allow-downgrade: %v", err)
	}
	// Same release (a re-run) and an unstamped dev build are not downgrades.
	for _, cur := range []string{"2026.11.07.3", "dev", "dev+gabc"} {
		if _, _, err := fetch(t, base, FetchOptions{Current: cur}); err != nil {
			t.Errorf("current %q: %v", cur, err)
		}
	}
}

func TestFetchRejectsAReplayedRelease(t *testing.T) {
	// An older, validly signed release served under a newer tag's URL.
	old := newRelease(t, "v2026.11.01.1", SignerIdentity)
	_, _, err := fetch(t, serve(t, old), FetchOptions{})
	if err == nil || !strings.Contains(err.Error(), "served under another tag") {
		t.Fatalf("want a replay refusal, got %v", err)
	}
}

func TestLoadVerifiedRechecksTheStagingDir(t *testing.T) {
	r := newRelease(t, testTag, SignerIdentity)
	got, v, err := fetch(t, serve(t, r), FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(got.Binary)
	if _, err := LoadVerified(context.Background(), v, dir, mustTag(t, testTag), "amd64"); err != nil {
		t.Fatalf("untouched staging dir: %v", err)
	}
	if err := os.WriteFile(got.Binary, fakeELF(elf.EM_X86_64, "edited after fetch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerified(context.Background(), v, dir, mustTag(t, testTag), "amd64"); err == nil {
		t.Fatal("a binary edited after fetch was accepted")
	}
}

// ── parsing ─────────────────────────────────────────────────────────────────

func TestParseTag(t *testing.T) {
	for _, bad := range []string{"", "latest", "main", "2026.11.07.1", "v2026.11.07.0", "v2026.11.07.1oops", "v2026.1.7.1", "v2026.11.07.1\nx"} {
		if _, err := ParseTag(bad); err == nil {
			t.Errorf("ParseTag(%q) accepted", bad)
		}
	}
	a, b := mustTag(t, "v2026.11.07.9"), mustTag(t, "v2026.11.07.10")
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 || a.Compare(mustTag(t, "v2026.11.07.9")) != 0 {
		t.Error(".10 must sort after .9")
	}
	if mustTag(t, "v2026.12.01.1").Compare(mustTag(t, "v2026.11.30.5")) <= 0 {
		t.Error("date must dominate the ordinal")
	}
	if a.String() != "v2026.11.07.9" {
		t.Errorf("String() = %q", a.String())
	}
	for in, want := range map[string]bool{"2026.11.07.2": true, "2026.11.07.2+3.gabc": true, "2026.11.07.2+dirty": true, "dev": false, "dev+gabc.dirty": false} {
		if _, ok := ParseVersion(in); ok != want {
			t.Errorf("ParseVersion(%q) ok=%v", in, ok)
		}
	}
}

func TestParseSums(t *testing.T) {
	good := strings.Repeat("a", 64) + "  fleet-linux-amd64\n" + strings.Repeat("b", 64) + " *release.json\n"
	s, err := ParseSums([]byte(good))
	if err != nil || len(s) != 2 || s[ManifestFile] != strings.Repeat("b", 64) {
		t.Fatalf("ParseSums: %v %v", s, err)
	}
	for name, bad := range map[string]string{
		"short":     "abc  x\n",
		"not hex":   strings.Repeat("z", 64) + "  x\n",
		"path":      strings.Repeat("a", 64) + "  ../x\n",
		"duplicate": strings.Repeat("a", 64) + "  x\n" + strings.Repeat("b", 64) + "  x\n",
		"empty":     "\n",
	} {
		if _, err := ParseSums([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseManifestRejectsUnpinnedImages(t *testing.T) {
	tag := mustTag(t, testTag)
	for _, ref := range []string{SandboxImage + ":latest", "ghcr.io/other/img@sha256:" + strings.Repeat("a", 64)} {
		raw, _ := json.Marshal(Manifest{Schema: 1, Tag: testTag, Commit: strings.Repeat("c", 40), Images: map[string]string{SandboxImage: ref}})
		if _, err := ParseManifest(raw, tag); err == nil {
			t.Errorf("image ref %q accepted", ref)
		}
	}
}

func TestCosignLock(t *testing.T) {
	pin := PinnedCosign()
	if !strings.HasPrefix(pin.Version, "v") {
		t.Errorf("version %q", pin.Version)
	}
	for _, a := range []string{"linux/amd64", "linux/arm64"} {
		if len(pin.SHA256[a]) != 64 {
			t.Errorf("no digest for %s", a)
		}
	}
}

func TestHealthURLFromAddr(t *testing.T) {
	for in, want := range map[string]string{
		"":               "http://127.0.0.1:8080/readyz",
		":9090":          "http://127.0.0.1:9090/readyz",
		"0.0.0.0:8080":   "http://127.0.0.1:8080/readyz",
		"10.0.0.5:8080":  "http://10.0.0.5:8080/readyz",
		"[::]:8080":      "http://127.0.0.1:8080/readyz",
		"localhost:8181": "http://localhost:8181/readyz",
	} {
		if got := HealthURLFromAddr(in); got != want {
			t.Errorf("HealthURLFromAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
