package models

import (
	"strings"
	"testing"
)

func TestValidateExecutionRequirements(t *testing.T) {
	block := func(body string) string {
		return "Refresh the page.\n" + ExecutionRequirementsMarker + "\n" + body + "\nFinish."
	}
	for _, ok := range []string{
		"An ordinary prompt with no declaration",
		block(`{"mcp_servers":["fast_io","pages"],"required_tools":["mcp_pages_get_page_data"],"network":true}`),
		block(`{"mcp_servers":["pages"],"mode":"refresh","completion":{"any_succeeded":["x"]}}`), // unknown keys ignored
		block(`{}`),
	} {
		if err := ValidateExecutionRequirements(ok); err != nil {
			t.Fatalf("valid prompt refused: %v\n%s", err, ok)
		}
	}
	for _, tc := range []struct{ prompt, want string }{
		{block(`{"mcp_servers":["fast_io + fastio_helpers","pages"]}`), `invalid server or tool identifier "fast_io + fastio_helpers"; allowed ^[a-zA-Z0-9_.-]{1,200}$`},
		{block(`{"required_tools":["mcp pages"]}`), `identifier "mcp pages"`},
		{block(`{"mcp_servers":"pages"}`), "invalid JSON object after the marker"},
		{block(`{bad}`), "invalid JSON object after the marker"},
		{block(`null`), "not a JSON object"},
		{block(`[]`), "invalid JSON object after the marker"},
		{"x\n" + ExecutionRequirementsMarker, "expected one bounded JSON object"},
		{block(`{}`) + "\n" + ExecutionRequirementsMarker + "\n{}", "expected one bounded JSON object"},
		{block(strings.Repeat("x", 16385)), "expected one bounded JSON object"},
		{block(`{"mcp_servers":[` + strings.Repeat(`"s",`, 100) + `"s"]}`), "too many servers or tools"},
	} {
		err := ValidateExecutionRequirements(tc.prompt)
		if err == nil || !strings.HasPrefix(err.Error(), "execution requirements: ") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got %v, want an execution requirements error containing %q", err, tc.want)
		}
	}
}
