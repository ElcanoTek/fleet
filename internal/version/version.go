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

import "runtime/debug"

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
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		rev += "+dirty"
	}
	return rev
}

// String renders the full build identity — "<version> (<revision>)" — for a
// --version / version affordance, e.g. "2026.09.04.2 (4e87891a2b3c)",
// "2026.09.04.2+3.g4e87891a2b3c (4e87891a2b3c)" or "dev (unknown)".
func String() string { return version + " (" + Revision() + ")" }
