// Package version exposes the fleet build identity — the release version and
// the VCS revision it was built from — to every fleet binary and surface.
//
// The single source of truth for the human-facing release number is the
// top-level VERSION file. The Makefile's `bins` target reads it and stamps it
// into the binary at build time via the linker:
//
//	go build -ldflags "-X github.com/ElcanoTek/fleet/internal/version.version=$(cat VERSION)" ./cmd/fleet
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

// version is the release number stamped in at build time from the top-level
// VERSION file via `-ldflags -X` (see the package doc + Makefile). It is a
// package-private var, not a const, precisely so the linker can override it; a
// const would be inlined and unpatchable. When no ldflag is supplied it keeps
// the "dev" sentinel so an unstamped binary is honestly labelled as such rather
// than claiming a release number it was not built from.
var version = "dev"

// Version returns the release version stamped from the VERSION file, or "dev"
// for a build that was not stamped (e.g. a bare `go build`).
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
// --version / version affordance, e.g. "0.0.0 (4e87891a2b3c)" or
// "dev (unknown)".
func String() string { return version + " (" + Revision() + ")" }
