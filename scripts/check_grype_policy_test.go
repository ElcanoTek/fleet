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
	tests := []struct {
		name     string
		report   string
		wantFail bool
		wantText string
	}{
		{name: "empty report passes", report: `{"matches":[]}`, wantText: "no fixable CRITICAL"},
		{
			name:     "upstream Python record remains report only",
			report:   grypeMatch("python", true),
			wantText: "no fixable CRITICAL",
		},
		{
			name:     "unfixed RPM remains report only",
			report:   grypeMatch("rpm", false),
			wantText: "no fixable CRITICAL",
		},
		{
			name:     "fixable critical RPM blocks",
			report:   grypeMatch("rpm", true),
			wantFail: true,
			wantText: "CVE-test  demo 1.0  fix: 2.0",
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

func grypeMatch(packageType string, fixable bool) string {
	fix := "[]"
	if fixable {
		fix = `["2.0"]`
	}
	return `{"matches":[{"vulnerability":{"id":"CVE-test","severity":"Critical","fix":{"versions":` + fix + `}},"artifact":{"type":"` + packageType + `","name":"demo","version":"1.0"}}]}`
}
