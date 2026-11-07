// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed cosign.lock
var cosignLockFile []byte

// CosignPin is one pinned cosign build.
type CosignPin struct {
	Version string
	// SHA256 maps "linux/<arch>" to the binary's digest.
	SHA256 map[string]string
}

// LoadCosignLock parses cosign.lock. The format is deliberately trivial
// (`version <v>` and `<os>/<arch> <sha256>` lines) so a shell can read it with
// awk; scripts/lib/release.sh does.
func LoadCosignLock(data []byte) (CosignPin, error) {
	pin := CosignPin{SHA256: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) != 2 {
			return CosignPin{}, fmt.Errorf("cosign.lock: malformed line %q", sc.Text())
		}
		if f[0] == "version" {
			pin.Version = f[1]
			continue
		}
		if len(f[1]) != 64 {
			return CosignPin{}, fmt.Errorf("cosign.lock: %s digest is not a SHA-256", f[0])
		}
		if _, err := hex.DecodeString(f[1]); err != nil {
			return CosignPin{}, fmt.Errorf("cosign.lock: %s digest is not hex", f[0])
		}
		pin.SHA256[f[0]] = f[1]
	}
	if pin.Version == "" || len(pin.SHA256) == 0 {
		return CosignPin{}, errors.New("cosign.lock: missing version or digests")
	}
	return pin, sc.Err()
}

// PinnedCosign is the cosign build compiled into this fleet.
func PinnedCosign() CosignPin {
	pin, err := LoadCosignLock(cosignLockFile)
	if err != nil {
		panic(err) // a malformed embedded lock is a build bug; TestCosignLock catches it
	}
	return pin
}

// cosignURL is where the pinned binary is fetched from. The digest, not the
// URL, is the trust anchor: a mirror (FLEET_COSIGN_URL) works as well.
func cosignURL(pin CosignPin, arch string) string {
	if base := strings.TrimRight(os.Getenv("FLEET_COSIGN_URL"), "/"); base != "" {
		return base + "/cosign-linux-" + arch
	}
	return "https://github.com/sigstore/cosign/releases/download/" + pin.Version + "/cosign-linux-" + arch
}

// EnsureCosign returns the path of the pinned cosign for arch under cacheDir,
// downloading it when absent. The file is used ONLY when its SHA-256 equals the
// pinned digest — checked on every call, not just after a download, so a cache
// that was tampered with or truncated is replaced rather than executed.
func EnsureCosign(ctx context.Context, d *Downloader, cacheDir, arch string) (string, error) {
	pin := PinnedCosign()
	want, ok := pin.SHA256["linux/"+arch]
	if !ok {
		return "", fmt.Errorf("no pinned cosign for linux/%s", arch)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("cosign cache: %w", err)
	}
	path := filepath.Join(cacheDir, "cosign-"+pin.Version+"-linux-"+arch)
	if got, err := fileSHA256(path); err == nil && got == want {
		return path, nil
	}
	tmp := path + ".download"
	if err := d.Get(ctx, cosignURL(pin, arch), tmp, maxCosignBytes); err != nil {
		return "", fmt.Errorf("download cosign %s: %w", pin.Version, err)
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("cosign %s for linux/%s has SHA-256 %s, cosign.lock pins %s — refusing to run it", pin.Version, arch, got, want)
	}
	if err := os.Chmod(tmp, 0o755); err != nil { //nolint:gosec // G302: an executable must be executable; it is root-owned in a 0700 dir.
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: paths are this package's own staging/cache files.
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verifier checks Sigstore signatures. Cosign is the production
// implementation; tests substitute fakes and a recorded-vector run.
type Verifier interface {
	// VerifyBlob verifies that bundle is a valid signature over the file at
	// path by the policy's identity.
	VerifyBlob(ctx context.Context, path, bundle string) error
	// VerifyImage verifies the image at a digest-pinned reference.
	VerifyImage(ctx context.Context, ref string) error
}

// Policy is who a signature must come from.
type Policy struct {
	Identity string
	Issuer   string
	// TrustedRoot, when set, is a Sigstore trusted_root.json to verify
	// against instead of the TUF-distributed public-good root — for
	// air-gapped boxes (FLEET_SIGSTORE_TRUSTED_ROOT) and for the recorded
	// test vectors.
	TrustedRoot string
}

// FleetPolicy is the policy every fleet release must satisfy.
func FleetPolicy() Policy {
	return Policy{Identity: SignerIdentity, Issuer: OIDCIssuer, TrustedRoot: os.Getenv("FLEET_SIGSTORE_TRUSTED_ROOT")}
}

// Cosign runs the cosign CLI.
type Cosign struct {
	Bin    string
	Policy Policy
	// TUFRoot is where cosign caches Sigstore's TUF metadata (TUF_ROOT).
	TUFRoot string
	Timeout time.Duration
}

func (c *Cosign) policyArgs() ([]string, error) {
	if c.Policy.Identity == "" || c.Policy.Issuer == "" {
		// Never a wildcard: an empty identity would make cosign refuse, but say
		// so here rather than depend on that.
		return nil, errors.New("verification policy has no identity/issuer")
	}
	args := []string{"--certificate-identity", c.Policy.Identity, "--certificate-oidc-issuer", c.Policy.Issuer}
	if c.Policy.TrustedRoot != "" {
		args = append(args, "--trusted-root", c.Policy.TrustedRoot)
	}
	return args, nil
}

// VerifyBlob implements Verifier.
func (c *Cosign) VerifyBlob(ctx context.Context, path, bundle string) error {
	if _, err := os.Stat(bundle); err != nil {
		return fmt.Errorf("no signature for %s: %w", filepath.Base(path), err)
	}
	pa, err := c.policyArgs()
	if err != nil {
		return err
	}
	args := append([]string{"verify-blob", "--bundle", bundle}, pa...)
	return c.run(ctx, append(args, path)...)
}

// VerifyImage implements Verifier.
func (c *Cosign) VerifyImage(ctx context.Context, ref string) error {
	if !digestRefRE.MatchString(ref) {
		return fmt.Errorf("refusing to verify %q: only a digest-pinned reference names one image", ref)
	}
	pa, err := c.policyArgs()
	if err != nil {
		return err
	}
	args := append([]string{"verify"}, pa...)
	return c.run(ctx, append(args, "--output", "text", ref)...)
}

func (c *Cosign) run(ctx context.Context, args ...string) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, args...) //nolint:gosec // G204: c.Bin is the digest-checked pinned cosign; args are fixed flags + paths.
	cmd.Env = os.Environ()
	if c.TUFRoot != "" {
		cmd.Env = append(cmd.Env, "TUF_ROOT="+c.TUFRoot)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("signature verification FAILED (cosign %s): %w: %s", args[0], err, msg)
	}
	return nil
}

// FileSHA256 is the lowercase hex SHA-256 of a file.
func FileSHA256(path string) (string, error) { return fileSHA256(path) }
