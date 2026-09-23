package models

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/truncate"
)

func TestValidateExecutionRequirements(t *testing.T) {
	block := func(body string) string {
		return "Refresh the page.\n" + ExecutionRequirementsMarker + "\n" + body + "\nFinish."
	}
	for _, ok := range []string{
		"An ordinary prompt with no declaration",
		block(`{"mcp_servers":["fast_io","pages"],"required_tools":["mcp_pages_get_page_data"],"network":true}`),
		block(`{"mcp_servers":["pages"],"mode":"refresh"}`), // unknown keys ignored
		block(`{"mcp_servers":["pages"],"completion":{"any_succeeded":["mcp_pages_record_refresh_check"]},"roster":"required_tools_only"}`),
		block(`{"roster":null,"completion":null}`),
		block(`{}`),
	} {
		if err := ValidateExecutionRequirements(ok); err != nil {
			t.Fatalf("valid prompt refused: %v\n%s", err, ok)
		}
	}
	for _, tc := range []struct{ prompt, want string }{
		{block(`{"mcp_servers":["fast_io + fastio_helpers","pages"]}`), `invalid server or tool identifier "fast_io + fastio_helpers" in mcp_servers[0]; allowed ^[a-zA-Z0-9_.-]{1,200}$`},
		{block(`{"required_tools":["mcp_pages_get_page_data","mcp pages"]}`), `identifier "mcp pages" in required_tools[1];`},
		// completion (#1602) and roster (#1603) follow the dispatch rules.
		{block(`{"completion":{"any_succeeded":["pages + record"]}}`), `identifier "pages + record" in completion.any_succeeded[0];`},
		// A long invalid name is quoted clamped, not whole.
		{block(`{"required_tools":["` + strings.Repeat("x", 300) + ` y"]}`), `identifier "` + truncate.Clamp(strings.Repeat("x", 300), 120, "…") + `" in required_tools[0]`},
		{block(`{"completion":["mcp_pages_record_refresh_check"]}`), "invalid JSON object after the marker"},
		{block(`{"completion":{"any_succeeded":"mcp_pages_record_refresh_check"}}`), "invalid JSON object after the marker"},
		{block(`{"roster":"everything"}`), `unknown roster "everything"`},
		{block(`{"roster":""}`), `unknown roster ""`},
		{block(`{"roster":["required_tools_only"]}`), "invalid JSON object after the marker"},
		{block(`{"mcp_servers":"pages"}`), "invalid JSON object after the marker"},
		{block(`{bad}`), "invalid JSON object after the marker"},
		{block(`null`), "not a JSON object"},
		{block(`[]`), "invalid JSON object after the marker"},
		// The three ways the line after the marker can be missing or unbounded,
		// each named, plus the blank line a copy-paste most often leaves.
		{"x\n" + ExecutionRequirementsMarker, "nothing follows the marker"},
		{block(`{}`) + "\n" + ExecutionRequirementsMarker + "\n{}", "marker appears more than once"},
		{block(strings.Repeat("x", 16385)), "the JSON line after the marker is 16385 bytes; at most 16384"},
		{block("") + "\n" + `{"mcp_servers":["pages"]}`, "the line after the marker is blank"},
		{block("  \r"), "the line after the marker is blank"},
		{block(`{"mcp_servers":[` + strings.Repeat(`"s",`, 100) + `"s"]}`), "too many servers or tools"},
	} {
		err := ValidateExecutionRequirements(tc.prompt)
		if err == nil || !strings.HasPrefix(err.Error(), "execution requirements: ") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got %v, want an execution requirements error containing %q", err, tc.want)
		}
	}
}

// ParseExecutionRequirements returns the declaration as data, so dispatch
// reads the same grammar into the same type.
func TestParseExecutionRequirements(t *testing.T) {
	req, err := ParseExecutionRequirements("TASK\n" + ExecutionRequirementsMarker + "\n" +
		`{"mcp_servers":["pages"],"required_tools":["mcp_pages_get_page_data"],"network":true,` +
		`"completion":{"any_succeeded":["mcp_pages_record_refresh_check"]},"roster":"required_tools_only","mode":"x"}` + "\nRun.")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(req.Servers, ",") != "pages" || strings.Join(req.Tools, ",") != "mcp_pages_get_page_data" || !req.Network ||
		req.Completion == nil || strings.Join(req.Completion.AnySucceeded, ",") != "mcp_pages_record_refresh_check" ||
		req.RosterNarrowing() != ExecutionRequirementsRosterRequiredToolsOnly {
		t.Fatalf("parsed = %+v", req)
	}
	for _, prompt := range []string{"An ordinary prompt", ExecutionRequirementsMarker + "\n" + `{"roster":null}`} {
		req, err := ParseExecutionRequirements(prompt)
		if err != nil || req.RosterNarrowing() != "" {
			t.Fatalf("%q: roster = %q, err %v; want no narrowing", prompt, req.RosterNarrowing(), err)
		}
	}
	if req, err := ParseExecutionRequirements("An ordinary prompt"); req != nil || err != nil {
		t.Fatalf("no marker must parse to (nil, nil), got %+v, %v", req, err)
	}
}
