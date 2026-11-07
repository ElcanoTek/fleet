// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Size ceilings for a download. Generous for what each file is; their job is
// to stop a hostile or broken server from filling the disk, not to police
// growth.
const (
	maxMetaBytes   = 4 << 20   // SHA256SUMS, release.json, signature bundles
	maxBinaryBytes = 512 << 20 // the fleet binary (~100 MB today)
	maxCosignBytes = 512 << 20
)

// DefaultBaseURL is where release assets are downloaded from:
// <base>/<tag>/<file>. FLEET_RELEASE_BASE_URL overrides it (a mirror); the
// signatures, not the URL, are what is trusted.
const DefaultBaseURL = "https://github.com/" + Repository + "/releases/download"

// DefaultLatestURL is the GitHub API answer to "which release is newest?".
const DefaultLatestURL = "https://api.github.com/repos/" + Repository + "/releases/latest"

// BaseURL resolves the asset base URL.
func BaseURL() string {
	if v := strings.TrimRight(os.Getenv("FLEET_RELEASE_BASE_URL"), "/"); v != "" {
		return v
	}
	return DefaultBaseURL
}

// Downloader fetches URLs to files with a size ceiling.
type Downloader struct {
	Client *http.Client
}

// NewDownloader returns a Downloader with conservative timeouts.
func NewDownloader() *Downloader {
	return &Downloader{Client: &http.Client{Timeout: 15 * time.Minute}}
}

// Get downloads url to path (mode 0600, never executable), refusing a body
// larger than limit.
func (d *Downloader) Get(ctx context.Context, url, path string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "fleet-update")
	resp, err := d.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{URL: url, Status: resp.StatusCode}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: path is under the caller's staging dir.
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if n > limit {
		_ = os.Remove(path)
		return fmt.Errorf("%s is larger than %d bytes — refusing it", url, limit)
	}
	return nil
}

// HTTPError is a non-200 answer.
type HTTPError struct {
	URL    string
	Status int
}

func (e *HTTPError) Error() string { return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Status) }

// IsNotFound reports whether err is a 404 — a release without that file.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// LatestTag asks the GitHub API which release is newest. Only a published
// release is "latest", and release.yml publishes the release object after its
// assets are uploaded, so this never names a tag whose files are still being
// built.
func (d *Downloader) LatestTag(ctx context.Context) (Tag, error) {
	url := DefaultLatestURL
	if v := os.Getenv("FLEET_RELEASE_LATEST_URL"); v != "" {
		url = v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) // #nosec G704 — the GitHub API, or the operator's FLEET_RELEASE_LATEST_URL
	if err != nil {
		return Tag{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "fleet-update")
	resp, err := d.Client.Do(req) // #nosec G704 — see above
	if err != nil {
		return Tag{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Tag{}, &HTTPError{URL: url, Status: resp.StatusCode}
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaBytes)).Decode(&body); err != nil {
		return Tag{}, fmt.Errorf("latest release: %w", err)
	}
	return ParseTag(body.TagName)
}

// FetchOptions selects and checks one release download.
type FetchOptions struct {
	Tag  Tag
	Arch string
	// Dir is the staging directory. It is created 0700 when missing and must
	// be private (see CheckPrivateDir): owned by this user, closed to
	// everyone else, and not replaceable through any of its parents.
	Dir     string
	BaseURL string
	// Current is the running fleet's version string; with AllowDowngrade
	// false, a Tag older than it is refused.
	Current        string
	AllowDowngrade bool
	// Commit, when set, is the commit the release tag resolves to in the
	// caller's checkout; the signed release.json must name the same one.
	Commit string
}

// Fetched is a verified release on disk.
type Fetched struct {
	Tag      Tag
	Binary   string // verified, executable, in the private staging dir
	Manifest Manifest
	Sums     Sums
	// BinaryFile is the binary held open from the moment it was verified
	// (LoadVerified only). Install copies from THIS descriptor, never from
	// the name again. The caller closes it.
	BinaryFile *os.File
	// BinarySHA256 is the binary's verified digest.
	BinarySHA256 string
}

// ErrDowngrade is returned when the requested release is older than the one
// running and AllowDowngrade is off.
var ErrDowngrade = errors.New("downgrade refused")

// Fetch downloads and verifies one release. Nothing it returns is used, and
// the binary is not even made executable, until every check has passed:
//
//  1. downgrade guard (before any download);
//  2. SHA256SUMS signature, by FleetPolicy's identity (a missing bundle is a
//     failure, never a skip) — checked on a private copy of the exact bytes
//     that are then parsed, so nothing can change them in between;
//  3. the binary's and release.json's SHA-256 against the verified sums,
//     each hashed from an open descriptor;
//  4. the binary's own signature bundle (on the same private copy);
//  5. release.json names the requested tag (replay guard) and, when the
//     caller gave one, the commit its checkout resolves that tag to;
//  6. the binary is an ELF for this architecture (a mislabelled asset fails
//     here even though its checksum and signature are fine).
func Fetch(ctx context.Context, d *Downloader, v Verifier, o FetchOptions) (*Fetched, error) {
	if !SupportedArch(o.Arch) {
		return nil, fmt.Errorf("no fleet release is published for linux/%s (amd64 and arm64 are)", o.Arch)
	}
	if cur, ok := ParseVersion(o.Current); ok && !o.AllowDowngrade && o.Tag.Compare(cur) < 0 {
		return nil, fmt.Errorf("%w: %s is older than the running %s (pass --allow-downgrade to roll back on purpose)", ErrDowngrade, o.Tag, cur)
	}
	if err := EnsurePrivateDir(o.Dir); err != nil {
		return nil, err
	}
	private, err := os.MkdirTemp("", "fleet-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(private)

	base := o.BaseURL
	if base == "" {
		base = BaseURL()
	}
	url := func(name string) string { return base + "/" + o.Tag.String() + "/" + name }
	get := func(name string, limit int64) error {
		if err := d.Get(ctx, url(name), filepath.Join(o.Dir, name), limit); err != nil {
			return fmt.Errorf("download %s: %w", name, err)
		}
		return nil
	}
	if err := get(SumsFile, maxMetaBytes); err != nil {
		return nil, err
	}
	if err := get(SumsFile+BundleSuffix, maxMetaBytes); err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("release %s has no signature for %s — refusing an unsigned release", o.Tag, SumsFile)
		}
		return nil, err
	}
	sums, err := verifiedSums(ctx, v, o.Dir, private)
	if err != nil {
		return nil, err
	}

	asset := BinaryAsset(o.Arch)
	if _, ok := sums[asset]; !ok {
		return nil, fmt.Errorf("the signed %s does not list %s", SumsFile, asset)
	}
	if err := get(asset, maxBinaryBytes); err != nil {
		return nil, err
	}
	if err := get(asset+BundleSuffix, maxMetaBytes); err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("release %s has no signature for %s — refusing it", o.Tag, asset)
		}
		return nil, err
	}
	bf, err := openVerifiedBinary(o.Dir, asset, sums[asset], o.Arch)
	if err != nil {
		return nil, err
	}
	defer bf.Close()
	// The binary's own signature, on a private copy of the verified bytes.
	binCopy := filepath.Join(private, asset)
	if err := copyFromFD(bf, binCopy, sums[asset]); err != nil {
		return nil, err
	}
	bundle, err := readPrivateFile(o.Dir, asset+BundleSuffix)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(binCopy+BundleSuffix, bundle, 0o600); err != nil {
		return nil, err
	}
	if err := v.VerifyBlob(ctx, binCopy, binCopy+BundleSuffix); err != nil {
		return nil, fmt.Errorf("%s: %w", asset, err)
	}

	if _, ok := sums[ManifestFile]; !ok {
		return nil, fmt.Errorf("the signed %s does not list %s", SumsFile, ManifestFile)
	}
	if err := get(ManifestFile, maxMetaBytes); err != nil {
		return nil, err
	}
	man, err := verifiedManifest(o.Dir, sums, o.Tag, o.Commit)
	if err != nil {
		return nil, err
	}
	if err := bf.Chmod(0o755); err != nil {
		return nil, err
	}
	return &Fetched{Tag: o.Tag, Binary: filepath.Join(o.Dir, asset), Manifest: man, Sums: sums, BinarySHA256: sums[asset]}, nil
}

// LoadVerified re-reads a staging dir that Fetch already verified, for the
// later steps of an update that run as separate invocations (the sandbox image
// and the install). It re-checks the signature on SHA256SUMS and every digest
// it relies on, so a staging dir edited between the steps is refused rather
// than trusted because an earlier process said so. The binary comes back as an
// OPEN descriptor whose bytes were hashed (Fetched.BinaryFile): installing
// from it, never from the name, leaves no window to swap the file after the
// check. The caller closes it.
func LoadVerified(ctx context.Context, v Verifier, dir string, tag Tag, arch string) (*Fetched, error) {
	if err := CheckPrivateDir(dir); err != nil {
		return nil, err
	}
	private, err := os.MkdirTemp("", "fleet-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(private)
	sums, err := verifiedSums(ctx, v, dir, private)
	if err != nil {
		return nil, err
	}
	m, err := verifiedManifest(dir, sums, tag, "")
	if err != nil {
		return nil, err
	}
	asset := BinaryAsset(arch)
	want, ok := sums[asset]
	if !ok {
		return nil, fmt.Errorf("the signed %s does not list %s", SumsFile, asset)
	}
	bf, err := openVerifiedBinary(dir, asset, want, arch)
	if err != nil {
		return nil, err
	}
	return &Fetched{Tag: tag, Binary: filepath.Join(dir, asset), Manifest: m, Sums: sums, BinaryFile: bf, BinarySHA256: want}, nil
}

// verifiedSums reads SHA256SUMS and its bundle from dir ONCE, writes those
// exact bytes to the private dir, verifies the signature there and parses the
// same bytes — the file cosign checked is the file that is used.
func verifiedSums(ctx context.Context, v Verifier, dir, private string) (Sums, error) {
	raw, err := readPrivateFile(dir, SumsFile)
	if err != nil {
		return nil, err
	}
	bundle, err := readPrivateFile(dir, SumsFile+BundleSuffix)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no signature for %s in %s — refusing an unsigned release", SumsFile, dir)
		}
		return nil, err
	}
	p := filepath.Join(private, SumsFile)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p+BundleSuffix, bundle, 0o600); err != nil {
		return nil, err
	}
	if err := v.VerifyBlob(ctx, p, p+BundleSuffix); err != nil {
		return nil, fmt.Errorf("%s: %w", SumsFile, err)
	}
	return ParseSums(raw)
}

// verifiedManifest reads release.json once, checks those bytes against the
// signed sums, and parses them.
func verifiedManifest(dir string, sums Sums, tag Tag, commit string) (Manifest, error) {
	want, ok := sums[ManifestFile]
	if !ok {
		return Manifest{}, fmt.Errorf("the signed %s does not list %s", SumsFile, ManifestFile)
	}
	raw, err := readPrivateFile(dir, ManifestFile)
	if err != nil {
		return Manifest{}, err
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want {
		return Manifest{}, fmt.Errorf("%s has SHA-256 %s but the signed %s says %s — refusing it", ManifestFile, got, SumsFile, want)
	}
	m, err := ParseManifest(raw, tag)
	if err != nil {
		return Manifest{}, err
	}
	if commit != "" && !strings.EqualFold(m.Commit, commit) {
		return Manifest{}, fmt.Errorf("%s says %s is commit %s, but the tag resolves to %s here — refusing a release whose tag was moved", ManifestFile, tag, m.Commit, commit)
	}
	return m, nil
}

// openVerifiedBinary opens dir/name without following links, requires a
// private regular file, hashes it from the descriptor and checks its ELF
// machine — and returns that descriptor, positioned at the start.
func openVerifiedBinary(dir, name, want, arch string) (*os.File, error) {
	f, err := openPrivate(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = f.Close()
		return nil, fmt.Errorf("%s has SHA-256 %s but the signed %s says %s — refusing it", name, got, SumsFile, want)
	}
	if err := checkELFArchFrom(f, name, arch); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// readPrivateFile reads a metadata file (up to maxMetaBytes) of dir/name
// through openPrivate.
func readPrivateFile(dir, name string) ([]byte, error) {
	const limit = maxMetaBytes
	f, err := openPrivate(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes — refusing it", name, limit)
	}
	return raw, nil
}

// openPrivate opens a staging file: no symlink at the last component
// (O_NOFOLLOW), a regular file, owned by this user and writable by no one
// else — a file someone else could still write to could change after it was
// hashed.
func openPrivate(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: a file in the checked private staging dir.
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || int(st.Uid) != os.Geteuid() || fi.Mode().Perm()&0o022 != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a private regular file owned by uid %d — refusing it", path, os.Geteuid())
	}
	return f, nil
}

// copyFromFD copies src (from its start) to a new private file at dst,
// requiring the copied bytes to hash to want.
func copyFromFD(src *os.File, dst, want string) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: our private temp dir.
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("the binary changed while it was being copied (%s, want %s)", got, want)
	}
	_, err = src.Seek(0, io.SeekStart)
	return err
}

// checkELFArchFrom refuses a binary built for another machine.
func checkELFArchFrom(r io.ReaderAt, name, arch string) error {
	f, err := elf.NewFile(r)
	if err != nil {
		return fmt.Errorf("%s is not an ELF executable: %w", name, err)
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if f.Machine != want {
		return fmt.Errorf("%s is built for %s, this box is linux/%s — refusing it", name, f.Machine, arch)
	}
	return nil
}

// EnsurePrivateDir creates dir (0700) when it is missing, tightens it to 0700
// when this user owns it, and then requires CheckPrivateDir.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Geteuid() && fi.Mode().Perm() != 0o700 {
			if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: tightening, not loosening.
				return err
			}
		}
	}
	return CheckPrivateDir(dir)
}

// CheckPrivateDir requires dir to be a directory owned by this user, closed to
// group and others (0700), and reachable only through directories nobody else
// can rename or replace entries in: every ancestor is owned by root or this
// user, and is either not group/other-writable or sticky (where others cannot
// rename an entry they do not own). Symlinks in the path are resolved first
// and the resolved chain is what is checked. A staging dir that fails this
// could have its verified files swapped after verification.
func CheckPrivateDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("staging dir %s: %w", dir, err)
	}
	euid := os.Geteuid()
	fi, err := os.Lstat(resolved)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != euid || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("staging dir %s must be a directory owned by uid %d with mode 0700 (is %v) — refusing it", resolved, euid, fi.Mode())
	}
	for p := filepath.Dir(resolved); ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || (st.Uid != 0 && int(st.Uid) != euid) {
			return fmt.Errorf("staging dir %s sits under %s, owned by uid %d — another user could replace it; use a dir under a root- or self-owned path", resolved, p, st.Uid)
		}
		if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("staging dir %s sits under %s (mode %v), which others can write to — they could replace it; use a dir under a path only root writes", resolved, p, fi.Mode())
		}
		if p == "/" || p == filepath.Dir(p) {
			return nil
		}
	}
}
