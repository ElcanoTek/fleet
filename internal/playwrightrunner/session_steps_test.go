package playwrightrunner

import (
	"context"
	"strings"
	"testing"
)

// TestRunStepsSecretPrecheckFollowsGotoSteps covers RunSteps' early refusal
// of a credential step, without a browser: a closed session runs every check
// first and only then reports that it is closed, so "browser session is
// closed" means the steps passed.
func TestRunStepsSecretPrecheckFollowsGotoSteps(t *testing.T) {
	newSession := func() *LiveSession {
		return &LiveSession{
			baseURL:     "https://a.example.com/login",
			currentURL:  "https://b.example.com/elsewhere",
			credentials: Credentials{Password: "pw-1928"},
			pending:     map[string]chan workerMessage{},
			closed:      true,
		}
	}
	fill := Step{Action: "fill", Selector: "#pw", Value: "{{PASSWORD}}"}
	ctx := context.Background()

	cases := []struct {
		name    string
		steps   []Step
		wantErr string
	}{
		{"credential step while the page is on another origin", []Step{fill},
			`refusing secret-bearing step "fill" after cross-origin navigation: the page is on https://b.example.com, not on this session's origin https://a.example.com`},
		{"a goto back to the session's origin comes first", []Step{{Action: "goto", Value: "https://a.example.com/form"}, fill},
			"browser session is closed"},
		{"a goto to another origin is refused before any fill", []Step{{Action: "goto", Value: "https://b.example.com/form"}, fill},
			"cannot navigate goto step to a different origin"},
		{"a step without a placeholder is not checked", []Step{{Action: "fill", Selector: "#q", Value: "hello"}},
			"browser session is closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newSession().RunSteps(ctx, tc.steps, false, false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RunSteps err = %v, want it to contain %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "pw-1928") {
				t.Fatalf("RunSteps error contains the password: %v", err)
			}
		})
	}
}
