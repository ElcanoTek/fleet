// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// Package release is the box side of fleet's signed release artifacts
// (docs/RELEASE-ARTIFACTS.md, ADR-0100): it resolves a release, downloads the
// fleet binary for this box's architecture, verifies it BEFORE anything uses it,
// swaps it in atomically while keeping the previous binary, restarts the
// service and rolls back when the new one does not become ready.
//
// The trust chain, end to end:
//
//  1. release.yml (a workflow_run on a green CI push to main) builds the
//     binaries and images, writes SHA256SUMS over every published file, and
//     signs SHA256SUMS, each binary and each image keylessly with cosign. The
//     signing certificate is issued by Sigstore's Fulcio to the GitHub OIDC
//     identity of that one workflow on that one branch.
//  2. This package verifies the signature on SHA256SUMS, and on the binary,
//     against EXACTLY that identity (SignerIdentity) and issuer (OIDCIssuer).
//     Any other signer — another workflow, another branch, a fork, a person —
//     fails, and so does a missing signature: there is no unverified mode.
//  3. Every other file is trusted only through its line in the verified
//     SHA256SUMS. release.json, covered the same way, carries the tag (so a
//     correctly signed OLD release served under a new tag is refused) and the
//     image digests (so the sandbox image pulled is the one this release
//     built, and its own cosign signature is checked on top).
//
// The verifier is the cosign CLI at a version pinned, with per-architecture
// SHA-256 digests, in cosign.lock (embedded here and read by
// scripts/lib/release.sh, so the Go updater and the shell bootstrap pin the
// same bytes). ADR-0100 records why cosign and not the sigstore-go library or
// `gh attestation verify`.
package release

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Repository is the GitHub repository releases are published from.
const Repository = "ElcanoTek/fleet"

// SignerIdentity is the ONE certificate identity a fleet release signature may
// carry: the release workflow, run from the default branch. release.yml is
// triggered by workflow_run, whose workflow ref is always the default branch,
// so this is the identity every legitimate release is signed with. A signature
// from any other workflow file, any other ref, or any other repository is
// refused — which is the property that makes "signed" mean "built by fleet's
// release pipeline" rather than "signed by someone with a GitHub account".
const SignerIdentity = "https://github.com/" + Repository + "/.github/workflows/release.yml@refs/heads/main"

// OIDCIssuer is GitHub Actions' OIDC issuer, the only issuer accepted for
// SignerIdentity.
const OIDCIssuer = "https://token.actions.githubusercontent.com"

// Image repositories release.yml publishes (lowercase: OCI names must be).
const (
	FleetImage   = "ghcr.io/elcanotek/fleet"
	SandboxImage = "ghcr.io/elcanotek/fleet-sandbox"
)

// Published file names. One binary per architecture; every file listed in
// SHA256SUMS is covered by its signature.
const (
	SumsFile       = "SHA256SUMS"
	ManifestFile   = "release.json"
	BundleSuffix   = ".sigstore.json" // cosign keyless bundle beside the file it signs
	binaryPrefix   = "fleet-linux-"
	manifestSchema = 1
)

// BinaryAsset is the published binary name for a GOARCH ("fleet-linux-amd64").
func BinaryAsset(arch string) string { return binaryPrefix + arch }

// SupportedArch reports whether release.yml publishes a binary for arch.
func SupportedArch(arch string) bool { return arch == "amd64" || arch == "arm64" }

// HostArch is the architecture this process runs on, which is the
// architecture of the box (a fleet binary is never run under emulation).
func HostArch() string { return runtime.GOARCH }

// Tag is a parsed release tag vYYYY.MM.DD.N (docs/VERSIONING.md).
type Tag struct {
	Year, Month, Day, N int
}

// tagRE matches a release tag exactly. scripts/version.sh owns the format;
// TestTagFormatMatchesVersionScript keeps the two in step.
var tagRE = regexp.MustCompile(`^v([0-9]{4})\.([0-9]{2})\.([0-9]{2})\.([1-9][0-9]*)$`)

// versionRE matches the leading release identity of a stamped build version
// ("2026.09.04.2", "2026.09.04.2+3.gabc", "2026.09.04.2+dirty").
var versionRE = regexp.MustCompile(`^v?([0-9]{4})\.([0-9]{2})\.([0-9]{2})\.([1-9][0-9]*)(?:\+.*)?$`)

// ParseTag parses an exact release tag. Anything else — a branch name, a SHA,
// "latest", a tag with a suffix — is refused: the tag selects which signed
// release is downloaded and is checked against the signed release.json, so it
// must be a value both sides can state exactly.
func ParseTag(s string) (Tag, error) {
	m := tagRE.FindStringSubmatch(s)
	if m == nil {
		return Tag{}, fmt.Errorf("%q is not a release tag (want vYYYY.MM.DD.N)", s)
	}
	return tagFrom(m), nil
}

// ParseVersion extracts the release a stamped build version is at or past.
// ok is false for an unstamped "dev" build, which has no release to compare.
func ParseVersion(v string) (Tag, bool) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return Tag{}, false
	}
	return tagFrom(m), true
}

func tagFrom(m []string) Tag {
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return Tag{Year: n(m[1]), Month: n(m[2]), Day: n(m[3]), N: n(m[4])}
}

// String renders the tag as published ("v2026.09.04.2").
func (t Tag) String() string {
	return fmt.Sprintf("v%04d.%02d.%02d.%d", t.Year, t.Month, t.Day, t.N)
}

// Compare orders tags by release date, then by the day's ordinal.
func (t Tag) Compare(o Tag) int {
	for _, d := range [][2]int{{t.Year, o.Year}, {t.Month, o.Month}, {t.Day, o.Day}, {t.N, o.N}} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	return 0
}

// Sums is a parsed SHA256SUMS: file name → lowercase hex digest.
type Sums map[string]string

// ParseSums parses `sha256sum` output strictly. A malformed line, a duplicate
// name or a path-shaped name is an error rather than a skipped line: the file
// is the root every other digest hangs off, so it is read as a contract.
func ParseSums(data []byte) (Sums, error) {
	out := Sums{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		if text == "" {
			continue
		}
		sum, name, ok := strings.Cut(text, "  ")
		if !ok {
			// sha256sum's binary-mode marker: "<hex> *<name>".
			sum, name, ok = strings.Cut(text, " *")
		}
		if !ok || len(sum) != 64 {
			return nil, fmt.Errorf("%s line %d: not a `sha256sum` line", SumsFile, line)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("%s line %d: digest is not hex", SumsFile, line)
		}
		if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return nil, fmt.Errorf("%s line %d: %q is not a plain file name", SumsFile, line, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s lists %q twice", SumsFile, name)
		}
		out[name] = strings.ToLower(sum)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New(SumsFile + " is empty")
	}
	return out, nil
}

// Manifest is release.json: the signed statement of what one release is.
type Manifest struct {
	Schema int    `json:"schema"`
	Tag    string `json:"tag"`
	Commit string `json:"commit"`
	// Images maps an image repository (FleetImage, SandboxImage) to its
	// digest-pinned reference, "<repo>@sha256:<hex>".
	Images map[string]string `json:"images"`
}

var digestRefRE = regexp.MustCompile(`^([a-z0-9][a-z0-9./_-]*)@sha256:[0-9a-f]{64}$`)

// ParseManifest decodes and validates release.json against the tag that was
// requested. A tag mismatch is the replay check: an older release's files are
// all validly signed, and served under a newer tag's URL they would otherwise
// pass every signature check.
func ParseManifest(data []byte, want Tag) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if m.Schema != manifestSchema {
		return Manifest{}, fmt.Errorf("%s: schema %d, this fleet understands %d", ManifestFile, m.Schema, manifestSchema)
	}
	if m.Tag != want.String() {
		return Manifest{}, fmt.Errorf("%s names release %q but %s was requested — refusing a release served under another tag", ManifestFile, m.Tag, want)
	}
	if len(m.Commit) != 40 {
		return Manifest{}, fmt.Errorf("%s: commit %q is not a full SHA", ManifestFile, m.Commit)
	}
	for repo, ref := range m.Images {
		sm := digestRefRE.FindStringSubmatch(ref)
		if sm == nil || sm[1] != repo {
			return Manifest{}, fmt.Errorf("%s: image %q → %q is not a digest-pinned reference to that repository", ManifestFile, repo, ref)
		}
	}
	return m, nil
}

// ImageRef returns the digest-pinned reference the manifest records for repo.
func (m Manifest) ImageRef(repo string) (string, error) {
	ref, ok := m.Images[repo]
	if !ok {
		return "", fmt.Errorf("release %s publishes no %s image", m.Tag, repo)
	}
	return ref, nil
}
