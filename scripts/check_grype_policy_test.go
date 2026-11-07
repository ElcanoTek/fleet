package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckGrypePolicy(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required by the CI policy script")
	}
	const rpmOwned = "/usr/lib/python3.14/site-packages/demo-1.0.dist-info/METADATA"
	const pipInstalled = "/usr/local/lib/python3.14/site-packages/demo-1.0.dist-info/METADATA"
	tests := []struct {
		name     string
		report   string
		wantFail bool
		wantText string
	}{
		{name: "empty report passes", report: `{"matches":[]}`, wantText: "no fixable CRITICAL"},
		{
			name:     "RPM-owned Python dist-info remains report only",
			report:   grypeMatch("python", "Critical", true, rpmOwned),
			wantText: "no fixable CRITICAL",
		},
		{
			name:     "Python record with no location remains report only",
			report:   grypeMatch("python", "Critical", true),
			wantText: "no fixable CRITICAL",
		},
		{
			name:     "unfixed RPM remains report only",
			report:   grypeMatch("rpm", "Critical", false),
			wantText: "no fixable CRITICAL",
		},
		{
			name:     "fixable critical RPM blocks",
			report:   grypeMatch("rpm", "Critical", true),
			wantFail: true,
			wantText: "CVE-test  rpm demo 1.0  fix: 2.0",
		},
		{
			name:     "fixable high pip package under /usr/local blocks",
			report:   grypeMatch("python", "High", true, pipInstalled),
			wantFail: true,
			wantText: "CVE-test  python demo 1.0  fix: 2.0",
		},
		{
			name:     "unfixed pip package under /usr/local remains report only",
			report:   grypeMatch("python", "Critical", false, pipInstalled),
			wantText: "no fixable CRITICAL",
		},
		{
			name:     "medium pip package under /usr/local remains report only",
			report:   grypeMatch("python", "Medium", true, pipInstalled),
			wantText: "no fixable CRITICAL",
		},
		{
			name:     "a record found in both places is gated",
			report:   grypeMatch("python", "Critical", true, rpmOwned, pipInstalled),
			wantFail: true,
			wantText: "python demo 1.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			if err := os.WriteFile(path, []byte(tt.report), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("./check-grype-policy.sh", path).CombinedOutput()
			if (err != nil) != tt.wantFail {
				t.Fatalf("failure = %v, want %v; output:\n%s", err, tt.wantFail, out)
			}
			if !strings.Contains(string(out), tt.wantText) {
				t.Errorf("output does not contain %q:\n%s", tt.wantText, out)
			}
		})
	}
}

func grypeMatch(packageType, severity string, fixable bool, locations ...string) string {
	fix := "[]"
	if fixable {
		fix = `["2.0"]`
	}
	locs := make([]string, 0, len(locations))
	for _, l := range locations {
		locs = append(locs, `{"path":"`+l+`","layerID":"sha256:0"}`)
	}
	return `{"matches":[{"vulnerability":{"id":"CVE-test","severity":"` + severity + `","fix":{"versions":` + fix + `,"state":"fixed"}},` +
		`"artifact":{"type":"` + packageType + `","name":"demo","version":"1.0","locations":[` + strings.Join(locs, ",") + `]}}]}`
}
