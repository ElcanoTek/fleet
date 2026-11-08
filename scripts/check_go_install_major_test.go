// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// A workflow's `--go-install "<path>@v${NAME_VERSION}"` is the fallback
// scripts/install-pinned-tool.sh builds from when GitHub release assets are
// down. Go puts a module's major version in its path (/v2 and up; nothing for
// v0 and v1), so the path must agree with the pinned major or `go install`
// fails in exactly the outage the fallback exists for. update-pins.sh refuses
// automatic major bumps of these pins; this test catches a hand edit that
// moves one end without the other.

var (
	goInstallRe    = regexp.MustCompile(`--go-install\s+"([^"@\s]+)@v\$\{([A-Z][A-Z0-9_]*)_VERSION\}"`)
	majorSegmentRe = regexp.MustCompile(`/v(\d+)(?:/|$)`)
)

// goInstallMajorMismatch says why PKG cannot be installed at VERSION's major,
// or "" when the path's /vN suffix agrees with it.
func goInstallMajorMismatch(pkg, version string) string {
	major := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 2)[0]
	segs := majorSegmentRe.FindAllStringSubmatch(pkg, -1)
	switch {
	case major == "0" || major == "1":
		if len(segs) > 0 {
			return fmt.Sprintf("%s has a /v%s suffix but %s is major %s, which takes none", pkg, segs[0][1], version, major)
		}
	case len(segs) == 0:
		return fmt.Sprintf("%s has no /v%s suffix but %s is major %s", pkg, major, version, major)
	case len(segs) > 1:
		return fmt.Sprintf("%s has more than one /vN segment", pkg)
	case segs[0][1] != major:
		return fmt.Sprintf("%s is major %s but the pin %s is major %s", pkg, segs[0][1], version, major)
	}
	return ""
}

func TestGoInstallFallbacksMatchThePinnedMajor(t *testing.T) {
	checked := 0
	for name, body := range workflowFiles(t) {
		for _, m := range goInstallRe.FindAllStringSubmatch(body, -1) {
			pin := regexp.MustCompile(`(?m)^\s+` + m[2] + `_VERSION:\s*'([^']+)'`).FindStringSubmatch(body)
			if pin == nil {
				t.Errorf("%s: --go-install %s@v${%s_VERSION} but the file pins no %s_VERSION", name, m[1], m[2], m[2])
				continue
			}
			checked++
			if why := goInstallMajorMismatch(m[1], pin[1]); why != "" {
				t.Errorf("%s: --go-install for %s: %s — the fallback would fail during a release-asset outage", name, m[2], why)
			}
		}
	}
	// The workflows carry seven today; zero means the matcher broke.
	if checked == 0 {
		t.Fatal("found no --go-install fallbacks in the workflows — the matcher is broken")
	}
	t.Logf("checked %d --go-install fallbacks", checked)
}

func TestGoInstallMajorMismatch(t *testing.T) {
	for _, tc := range []struct {
		pkg, version string
		ok           bool
	}{
		{"github.com/zricethezav/gitleaks/v8", "8.30.1", true},
		{"github.com/golangci/golangci-lint/v2/cmd/golangci-lint", "2.14.0", true},
		{"github.com/yannh/kubeconform/cmd/kubeconform", "0.6.7", true},
		{"github.com/rhysd/actionlint/cmd/actionlint", "1.7.7", true},
		{"github.com/zricethezav/gitleaks/v8", "9.0.0", false},                      // bumped across a major, path left behind
		{"github.com/golangci/golangci-lint/v2/cmd/golangci-lint", "1.64.8", false}, // /v2 on a v1 pin
		{"github.com/anchore/grype/cmd/grype", "2.0.0", false},                      // v2 needs /v2
		{"example.com/a/v2/b/v3", "3.0.0", false},                                   // ambiguous path
		{"github.com/anchore/grype/cmd/grype", "0.117.0", true},
	} {
		why := goInstallMajorMismatch(tc.pkg, tc.version)
		if (why == "") != tc.ok {
			t.Errorf("goInstallMajorMismatch(%q, %q) = %q, want ok=%v", tc.pkg, tc.version, why, tc.ok)
		}
	}
}
