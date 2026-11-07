package envreg

import (
	"regexp"
	"testing"
)

var (
	settingName = regexp.MustCompile(`\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b`)
	// inheritsPhrase finds prose that says a setting's default comes from
	// another setting.
	inheritsPhrase = regexp.MustCompile(`(?i)\b(defaults? to|falls? back to|inherits?(?: from)?|derived from)\s+` + "`?" + `([A-Z][A-Z0-9]*_[A-Z0-9_]+)`)
)

// TestDerivedDefaultsAreStructured — Codex round 3 (#2057): a default that
// comes from another setting must be declared with DefaultFrom (so explain
// resolves it), never only described in text. So:
//   - a Default never names a registered setting;
//   - a Description that says "defaults to / falls back to / inherits /
//     derived from <SETTING>" must have DefaultFrom = that setting.
func TestDerivedDefaultsAreStructured(t *testing.T) {
	for _, e := range All() {
		for _, n := range settingName.FindAllString(e.Default, -1) {
			if IsRegistered(n) {
				t.Errorf("%s: Default %q names the setting %s; declare DefaultFrom instead", e.Name, e.Default, n)
			}
		}
		for _, m := range inheritsPhrase.FindAllStringSubmatch(e.Description, -1) {
			if IsRegistered(m[2]) && e.DefaultFrom != m[2] {
				t.Errorf("%s: the description says its default comes from %s (%q) but DefaultFrom is %q", e.Name, m[2], m[0], e.DefaultFrom)
			}
		}
		if e.DefaultDerive != nil && e.DefaultFrom == "" {
			t.Errorf("%s: DefaultDerive without DefaultFrom", e.Name)
		}
	}
}

func TestDefaultDerives(t *testing.T) {
	for _, tc := range []struct {
		f      func(string) (string, bool)
		in     string
		want   string
		wantOK bool
	}{
		{drainFileFromEnvFile, "/etc/fleet/fleet.env", "/etc/fleet/fleet.env.update.drain", true},
		{drainFileFromEnvFile, ".env.local", "", false},
		{drainFileFromEnvFile, "", "", false},
		{auditDirFromDataDir, "/var/lib/fleet", "/var/lib/fleet/audit", true},
		{auditDirFromDataDir, "", "", false},
		{oidcCallbackFromOrigin, "https://fleet.example.com/app", "https://fleet.example.com/api/auth/oidc/callback", true},
		{oidcCallbackFromOrigin, "not a url", "", false},
		{warmSizeFromMaxConcurrent, "1", "2", true},
		{warmSizeFromMaxConcurrent, "5", "5", true},
		{warmSizeFromMaxConcurrent, "40", "8", true},
		{warmSizeFromMaxConcurrent, "x", "", false},
	} {
		if got, ok := tc.f(tc.in); got != tc.want || ok != tc.wantOK {
			t.Errorf("derive(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
