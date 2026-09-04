// Package version exposes the fleet build identity — the release version and
// the VCS revision it was built from — to every fleet binary and surface.
//
// Releases are DATE-BASED and derived from git; there is no VERSION file and no
// hand-authored number anywhere (ADR-0059, docs/VERSIONING.md). Every green push
// to main is tagged vYYYY.MM.DD.N automatically, and the Makefile's `bins`
// target stamps what scripts/version.sh derives from those tags into the binary
// at build time via the linker:
//
//	go build -ldflags "-X github.com/ElcanoTek/fleet/internal/version.version=$(scripts/version.sh describe)" ./cmd/fleet
//
// So the stamped string is "2026.09.04.2" at a release, "2026.09.04.2+3.g<sha>"
// three commits past one — which is what a box tracking main between releases
// honestly is — with a ".dirty" marker for a modified tree.
//
// Builds that skip that ldflag — a bare `go build ./...`, `go run`, CI's
// compile-check step, `go test` — carry no stamped version, so version falls
// back to the "dev" sentinel below. In every case the VCS revision is recovered
// from the Go toolchain's embedded build info (runtime/debug.ReadBuildInfo),
// which the compiler records automatically when building inside a git checkout.
//
// This package deliberately holds NO other state and imports only the standard
// library: it is a leaf that any other package can depend on without creating a
// cycle.
package version

import (
	"runtime/debug"
	"strings"
)

// version is the release number stamped in at build time from the release tags
// via `-ldflags -X` (see the package doc + Makefile). It is a
// package-private var, not a const, precisely so the linker can override it; a
// const would be inlined and unpatchable. When no ldflag is supplied it keeps
// the "dev" sentinel so an unstamped binary is honestly labelled as such rather
// than claiming a release number it was not built from.
var version = "dev"

// Version returns the date-based release version stamped from the release tags,
// or "dev" for a build that was not stamped (e.g. a bare `go build`).
func Version() string { return version }

// Revision returns the short (12-hex-digit) VCS revision the binary was built
// from, recovered from the Go toolchain's embedded build info, or "unknown" when
// it is unavailable (e.g. building from an unpacked source tarball with no .git,
// or with -buildvcs=false). A "+dirty" suffix marks a build from a tree with
// uncommitted changes.
func Revision() string {
	rev, dirty := vcsRevision()
	if rev == "" {
		return "unknown"
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}

// vcsRevision reads the short revision and the modified flag out of the
// toolchain's embedded build info. rev is "" when the build info carries no VCS
// record at all.
func vcsRevision() (rev string, dirty bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev, dirty
}

// String renders the full build identity for a --version / version affordance,
// the login banner and the admin health summary. It is the stamped version plus
// the VCS revision — but each fact appears ONCE:
//
//	2026.09.04.2 (4e87891a2b3c)         exactly a release
//	2026.09.04.2+dirty (4e87891a2b3c)   at a release, tree modified
//	2026.09.04.2+3.g4e87891a2b3c        3 commits past a release
//	dev+g4e87891a2b3c.dirty             no release tag reachable, tree modified
//	dev (4e87891a2b3c)                  unstamped build (bare `go build`)
//	dev                                 unstamped, and no VCS info either
//
// The stamp and the build info are read from the same tree at the same build,
// so when scripts/version.sh already put the commit into the version
// ("+<n>.g<sha>" / "dev+g<sha>", each with its own ".dirty" marker) the
// revision would only repeat it: "dev+g<sha>.dirty (<sha>+dirty)" was what an
// untagged, modified checkout used to print. The revision is appended only when
// the version does not carry it, and its "+dirty" only when the version does
// not already say so.
func String() string {
	rev, dirty := vcsRevision()
	if rev == "" {
		return version
	}
	if strings.Contains(version, "g"+rev) {
		// The stamp names the commit (and its dirty state) already.
		return version
	}
	if dirty && !stampSaysDirty(version) {
		rev += "+dirty"
	}
	return version + " (" + rev + ")"
}

// stampSaysDirty reports whether the stamped version already carries
// scripts/version.sh's dirty marker — the ".dirty" metadata part, or the
// bare "+dirty" it becomes when it is the only part.
func stampSaysDirty(v string) bool {
	return strings.HasSuffix(v, "+dirty") || strings.HasSuffix(v, ".dirty")
}
