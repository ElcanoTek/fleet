package version

import (
	"regexp"
	"strings"
	"testing"
)

// TestVersionDefault: an unstamped build (the test binary is built without the
// release ldflag) reports the "dev" sentinel rather than a fabricated release
// number.
func TestVersionDefault(t *testing.T) {
	if got := Version(); got != "dev" {
		t.Errorf("Version() = %q, want %q (no ldflag stamped in tests)", got, "dev")
	}
}

// TestRevisionShape: the build-info revision is either absent or a short hex
// revision of at most 12 digits. It must not panic regardless of build-info
// availability.
func TestRevisionShape(t *testing.T) {
	rev, _ := vcsRevision()
	if rev == "" {
		return
	}
	shape := regexp.MustCompile(`^[0-9a-f]{1,12}$`)
	if !shape.MatchString(rev) {
		t.Errorf("vcsRevision() = %q, want short hex", rev)
	}
}

// TestString: the unstamped test binary renders "dev" plus the parenthesised
// revision when the toolchain recorded one, and bare "dev" when it did not —
// never a fabricated placeholder.
func TestString(t *testing.T) {
	s := String()
	rev, dirty := vcsRevision()
	if rev == "" {
		if s != "dev" {
			t.Errorf("String() = %q, want %q when no VCS info is available", s, "dev")
		}
		return
	}
	if dirty {
		rev += "+dirty"
	}
	if want := "dev (" + rev + ")"; s != want {
		t.Errorf("String() = %q, want %q", s, want)
	}
}

// TestStringDoesNotRepeatTheRevision pins the rendering rules against every
// shape scripts/version.sh can stamp. The bug this guards: an untagged, modified
// checkout printed "dev+g<sha>.dirty (<sha>+dirty)" — the same commit and the
// same dirty flag, twice, in the login banner and `fleet version`.
func TestStringDoesNotRepeatTheRevision(t *testing.T) {
	rev, dirty := vcsRevision()
	if rev == "" {
		t.Skip("no VCS build info in this test binary (built outside a git checkout or with -buildvcs=false)")
	}
	other := "0123456789ab" // a revision the stamp names that is NOT this build's
	if other == rev {
		other = "ba9876543210"
	}
	revDirty := rev
	if dirty {
		revDirty += "+dirty"
	}
	cases := []struct {
		name, stamp, want string
	}{
		// The stamp already names this commit: nothing to add.
		{"past-release", "2026.09.04.2+3.g" + rev, "2026.09.04.2+3.g" + rev},
		{"past-release-dirty", "2026.09.04.2+3.g" + rev + ".dirty", "2026.09.04.2+3.g" + rev + ".dirty"},
		{"untagged", "dev+g" + rev, "dev+g" + rev},
		{"untagged-dirty", "dev+g" + rev + ".dirty", "dev+g" + rev + ".dirty"},
		// The stamp carries no commit: the revision is appended once, and its
		// dirty flag only when the stamp does not already say so.
		{"exact-release", "2026.09.04.2", "2026.09.04.2 (" + revDirty + ")"},
		{"exact-release-dirty", "2026.09.04.2+dirty", "2026.09.04.2+dirty (" + rev + ")"},
		{"unstamped", "dev", "dev (" + revDirty + ")"},
		// A stamp naming a DIFFERENT commit (a stale ldflag) is not "already
		// covered": the real revision is still appended so the mismatch shows.
		{"stale-stamp", "2026.09.04.2+1.g" + other, "2026.09.04.2+1.g" + other + " (" + revDirty + ")"},
	}
	saved := version
	defer func() { version = saved }()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version = tc.stamp
			if got := String(); got != tc.want {
				t.Errorf("String() with stamp %q = %q, want %q", tc.stamp, got, tc.want)
			}
			if got := String(); strings.Count(got, rev) > 1 {
				t.Errorf("String() = %q repeats the revision %q", got, rev)
			}
		})
	}
}
