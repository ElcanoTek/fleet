// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	// Dir is the staging directory; it must exist and should be on the same
	// filesystem as the install dir so the swap is a rename.
	Dir     string
	BaseURL string
	// Current is the running fleet's version string; with AllowDowngrade
	// false, a Tag older than it is refused.
	Current        string
	AllowDowngrade bool
}

// Fetched is a verified release on disk.
type Fetched struct {
	Tag      Tag
	Binary   string // verified, executable
	Manifest Manifest
	Sums     Sums
}

// ErrDowngrade is returned when the requested release is older than the one
// running and AllowDowngrade is off.
var ErrDowngrade = errors.New("downgrade refused")

// Fetch downloads and verifies one release. Nothing it returns is used, and
// the binary is not even made executable, until every check has passed:
//
//  1. downgrade guard (before any download);
//  2. SHA256SUMS signature, by FleetPolicy's identity (a missing bundle is a
//     failure, never a skip);
//  3. the binary's and release.json's SHA-256 against the verified sums;
//  4. the binary's own signature bundle;
//  5. release.json names the requested tag (replay guard);
//  6. the binary is an ELF for this architecture (a mislabelled asset fails
//     here even though its checksum and signature are fine).
func Fetch(ctx context.Context, d *Downloader, v Verifier, o FetchOptions) (*Fetched, error) {
	if !SupportedArch(o.Arch) {
		return nil, fmt.Errorf("no fleet release is published for linux/%s (amd64 and arm64 are)", o.Arch)
	}
	if cur, ok := ParseVersion(o.Current); ok && !o.AllowDowngrade && o.Tag.Compare(cur) < 0 {
		return nil, fmt.Errorf("%w: %s is older than the running %s (pass --allow-downgrade to roll back on purpose)", ErrDowngrade, o.Tag, cur)
	}
	base := o.BaseURL
	if base == "" {
		base = BaseURL()
	}
	url := func(name string) string { return base + "/" + o.Tag.String() + "/" + name }
	get := func(name string, limit int64) (string, error) {
		p := filepath.Join(o.Dir, name)
		if err := d.Get(ctx, url(name), p, limit); err != nil {
			return "", fmt.Errorf("download %s: %w", name, err)
		}
		return p, nil
	}

	sumsPath, err := get(SumsFile, maxMetaBytes)
	if err != nil {
		return nil, err
	}
	sumsBundle, err := get(SumsFile+BundleSuffix, maxMetaBytes)
	if err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("release %s has no signature for %s — refusing an unsigned release", o.Tag, SumsFile)
		}
		return nil, err
	}
	if err := v.VerifyBlob(ctx, sumsPath, sumsBundle); err != nil {
		return nil, fmt.Errorf("%s: %w", SumsFile, err)
	}
	raw, err := os.ReadFile(sumsPath) //nolint:gosec // G304: our staging file.
	if err != nil {
		return nil, err
	}
	sums, err := ParseSums(raw)
	if err != nil {
		return nil, err
	}

	checked := func(name string, limit int64) (string, error) {
		want, ok := sums[name]
		if !ok {
			return "", fmt.Errorf("the signed %s does not list %s", SumsFile, name)
		}
		p, err := get(name, limit)
		if err != nil {
			return "", err
		}
		got, err := fileSHA256(p)
		if err != nil {
			return "", err
		}
		if got != want {
			_ = os.Remove(p)
			return "", fmt.Errorf("%s has SHA-256 %s but the signed %s says %s — refusing it", name, got, SumsFile, want)
		}
		return p, nil
	}

	asset := BinaryAsset(o.Arch)
	bin, err := checked(asset, maxBinaryBytes)
	if err != nil {
		return nil, err
	}
	binBundle, err := get(asset+BundleSuffix, maxMetaBytes)
	if err != nil {
		if IsNotFound(err) {
			return nil, fmt.Errorf("release %s has no signature for %s — refusing it", o.Tag, asset)
		}
		return nil, err
	}
	if err := v.VerifyBlob(ctx, bin, binBundle); err != nil {
		return nil, fmt.Errorf("%s: %w", asset, err)
	}
	manPath, err := checked(ManifestFile, maxMetaBytes)
	if err != nil {
		return nil, err
	}
	manRaw, err := os.ReadFile(manPath) //nolint:gosec // G304: our staging file.
	if err != nil {
		return nil, err
	}
	man, err := ParseManifest(manRaw, o.Tag)
	if err != nil {
		return nil, err
	}
	if err := checkELFArch(bin, o.Arch); err != nil {
		return nil, err
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // G302: the verified binary is about to be installed as an executable.
		return nil, err
	}
	return &Fetched{Tag: o.Tag, Binary: bin, Manifest: man, Sums: sums}, nil
}

// checkELFArch refuses a binary built for another machine.
func checkELFArch(path, arch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s is not an ELF executable: %w", filepath.Base(path), err)
	}
	defer f.Close()
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if f.Machine != want {
		return fmt.Errorf("%s is built for %s, this box is linux/%s — refusing it", filepath.Base(path), f.Machine, arch)
	}
	return nil
}

// LoadVerified re-reads a staging dir that Fetch already verified, for the
// later steps of an update that run as separate invocations (the sandbox image
// and the install). It re-checks the signature on SHA256SUMS and every digest
// it relies on, so a staging dir edited between the steps is refused rather
// than trusted because an earlier process said so.
func LoadVerified(ctx context.Context, v Verifier, dir string, tag Tag, arch string) (*Fetched, error) {
	sumsPath := filepath.Join(dir, SumsFile)
	if err := v.VerifyBlob(ctx, sumsPath, sumsPath+BundleSuffix); err != nil {
		return nil, fmt.Errorf("%s: %w", SumsFile, err)
	}
	raw, err := os.ReadFile(sumsPath) //nolint:gosec // G304: operator-named staging dir.
	if err != nil {
		return nil, err
	}
	sums, err := ParseSums(raw)
	if err != nil {
		return nil, err
	}
	check := func(name string) (string, error) {
		p := filepath.Join(dir, name)
		got, err := fileSHA256(p)
		if err != nil {
			return "", err
		}
		if want := sums[name]; got != want {
			return "", fmt.Errorf("%s in %s no longer matches the signed %s", name, dir, SumsFile)
		}
		return p, nil
	}
	man, err := check(ManifestFile)
	if err != nil {
		return nil, err
	}
	manRaw, err := os.ReadFile(man) //nolint:gosec // G304: checked above.
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(manRaw, tag)
	if err != nil {
		return nil, err
	}
	bin, err := check(BinaryAsset(arch))
	if err != nil {
		return nil, err
	}
	if err := checkELFArch(bin, arch); err != nil {
		return nil, err
	}
	return &Fetched{Tag: tag, Binary: bin, Manifest: m, Sums: sums}, nil
}
