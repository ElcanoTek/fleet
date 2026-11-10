package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func summaryRows(t *testing.T, summary map[string]any) [][2]string {
	t.Helper()
	rows, ok := summary["args"].([]map[string]string)
	if !ok {
		t.Fatalf("summary has no rows: %#v", summary)
	}
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{r["key"], r["value"]})
	}
	return out
}

func expectRows(t *testing.T, got, want [][2]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rows =\n%q\nwant\n%q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBrowserApprovalSummaryReadsInWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, tool, raw string
		want            [][2]string
	}{
		{
			name: "connector_studio steps in a live session with a vaulted password",
			tool: tools.ConnectorStudioToolName,
			raw: `{"action":"probe_browser","session_id":"s1","url":"https://a.example.com/form","password_ref":"$secret:password","steps":[` +
				`{"action":"fill","selector":"#pw","value":"{{PASSWORD}}"},{"action":"click","selector":"#go"},` +
				`{"action":"wait_for","timeout_ms":2000},{"action":"fill","selector":"#note","value":"hi {{USERNAME}}"},` +
				`{"action":"press","selector":"#q","value":"Enter","optional":true},{"action":"extract","selector":"h1"}]}`,
			want: [][2]string{
				{"What", "Run these steps in your live browser session"},
				{"Live session", "s1"},
				{"Goes to", "https://a.example.com/form"},
				{"Step 1", "Type the password into #pw"},
				{"Step 2", "Click #go"},
				{"Step 3", "Wait 2000 ms"},
				{"Step 4", `Type "hi <username>" into #note`},
				{"Step 5", "Press Enter in #q (if it is there)"},
				{"Step 6", "Read h1"},
				{"Password", `from the "password" form field`},
				{"Also", "lists the page's fields, buttons and links"},
			},
		},
		{
			name: "playwright_browser steps: url is not opened",
			tool: tools.PlaywrightBrowserToolName,
			raw:  `{"action":"steps","session_id":"s1","url":"https://a.example.com/","steps":[{"action":"click","selector":"#go"}]}`,
			want: [][2]string{
				{"What", "Run these steps in your live browser session"},
				{"Live session", "s1"},
				{"Step 1", "Click #go"},
			},
		},
		{
			name: "a one-shot run, even with a session_id",
			tool: tools.PlaywrightBrowserToolName,
			raw:  `{"action":"run","session_id":"s1","url":"https://a.example.com/login","username_ref":"alice","inspect_dom":true,"capture_network":true,"steps":[{"action":"goto","value":"https://a.example.com/x"}]}`,
			want: [][2]string{
				{"What", "Run these steps in a new headless browser"},
				{"Opens", "https://a.example.com/login"},
				{"Step 1", "Go to https://a.example.com/x"},
				{"Username", "alice"},
				{"Also", "lists the page's fields, buttons and links; records the data requests the page makes"},
			},
		},
		{
			name: "starting a live session, even with a session_id",
			tool: tools.ConnectorStudioToolName,
			raw:  `{"action":"start_copilot_session","session_id":"old","url":"https://a.example.com/login"}`,
			want: [][2]string{
				{"What", "Open a live browser session you can watch and use"},
				{"Opens", "https://a.example.com/login"},
				{"Also", "lists the page's fields, buttons and links"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectRows(t, summaryRows(t, summarizeApprovalInput(tc.tool, tc.raw, "")), tc.want)
		})
	}
}

// TestBrowserApprovalSummaryHidesCredentialValues: a credential given as a
// plain value is named, not printed: not in its own row, not in a step that
// types it, not in a step that types a literal into a password-like field.
func TestBrowserApprovalSummaryHidesCredentialValues(t *testing.T) {
	t.Parallel()
	raw := `{"action":"run","url":"https://a.example.com/","password_ref":"hunter2-plain-1928","steps":[` + // gitleaks:allow (made-up test value)
		`{"action":"fill","selector":"#login-pw","value":"hunter2-plain-1928"},` +
		`{"action":"fill","selector":"input[type=password]","value":"other-literal-1928"},` +
		`{"action":"fill","selector":"#otp","value":"123456"},` +
		`{"action":"type","selector":"#x","value":"raw-value-1928"}]}`
	summary := summarizeApprovalInput(tools.PlaywrightBrowserToolName, raw, "")
	b, _ := json.Marshal(summary)
	for _, secret := range []string{"hunter2-plain-1928", "other-literal-1928", "123456", "raw-value-1928"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the card shows %q: %s", secret, b)
		}
	}
	rows := summaryRows(t, summary)
	if rows[2][1] != "Type a value hidden here into #login-pw" || rows[5][1] != `"type" on #x (not a known step)` {
		t.Errorf("rows = %q", rows)
	}
	if !strings.Contains(string(b), "given in the chat (not shown here)") {
		t.Errorf("the card does not say where the password came from: %s", b)
	}
}

// TestNonBrowserConnectorStudioCallsKeepTheGenericCard: only the browser
// actions get the browser summary.
func TestNonBrowserConnectorStudioCallsKeepTheGenericCard(t *testing.T) {
	t.Parallel()
	got := summaryRows(t, summarizeApprovalInput(tools.ConnectorStudioToolName, `{"action":"create_custom_connector","name":"x"}`, ""))
	if got[0] != [2]string{"action", "create_custom_connector"} {
		t.Errorf("rows = %q, want the generic argument rows", got)
	}
}

// TestBrowserApprovalCardShowsTheLiveSessionSite: a pending card for steps in
// a live session says where the session started and where its page is now,
// and warns when they differ; a session start or a one-shot run is not looked
// up. Not parallel: it swaps browserSessionOrigins.
func TestBrowserApprovalCardShowsTheLiveSessionSite(t *testing.T) {
	orig := browserSessionOrigins
	t.Cleanup(func() { browserSessionOrigins = orig })
	sessions := map[string][2]string{
		"same":  {"https://a.example.com/login", "https://a.example.com/secure?token=abc"},
		"moved": {"https://a.example.com/login", "https://b.example.com/form"},
	}
	var lookups []string
	browserSessionOrigins = func(id, owner string) (string, string, bool) {
		lookups = append(lookups, id+"/"+owner)
		s, ok := sessions[id]
		return s[0], s[1], ok
	}
	card := func(tool, raw string) [][2]string {
		ev := approvalRequiredEvent(&store.Approval{ID: "a", ToolName: tool, UserEmail: "alice@example.com", ArgsJSON: raw}, raw, "c1")
		return summaryRows(t, ev["summary"].(map[string]any))
	}
	steps := func(id string) string {
		return `{"action":"probe_browser","session_id":"` + id + `","inspect_dom":true,"steps":[{"action":"click","selector":"#go"}]}`
	}

	expectRows(t, card(tools.ConnectorStudioToolName, steps("same")), [][2]string{
		{"What", "Run these steps in your live browser session"},
		{"Session started on", "https://a.example.com"},
		{"Page is now on", "https://a.example.com"},
		{"Live session", "same"},
		{"Step 1", "Click #go"},
		{"Also", "lists the page's fields, buttons and links"},
	})
	if len(lookups) != 1 || lookups[0] != "same/alice@example.com" {
		t.Errorf("lookups = %q, want the session looked up for the approval's user", lookups)
	}

	moved := card(tools.ConnectorStudioToolName, steps("moved"))
	if moved[2] != [2]string{"Page is now on", "https://b.example.com"} || moved[3][0] != "Note" ||
		!strings.Contains(moved[3][1], "A step that types a credential runs only while the page is on that site") {
		t.Errorf("a moved session's rows = %q, want the current site and a note", moved)
	}
	gone := card(tools.ConnectorStudioToolName, steps("ended"))
	if gone[1] != [2]string{"Session status", "ended or expired: approving will fail; ask for a new session"} {
		t.Errorf("an ended session's rows = %q, want its status", gone)
	}

	lookups = nil
	card(tools.ConnectorStudioToolName, `{"action":"start_copilot_session","session_id":"same","url":"https://a.example.com/","steps":[{"action":"click","selector":"#go"}]}`)
	card(tools.PlaywrightBrowserToolName, `{"action":"run","session_id":"same","url":"https://a.example.com/","steps":[{"action":"click","selector":"#go"}]}`)
	if len(lookups) != 0 {
		t.Errorf("a session start or a one-shot run looked up a session: %q", lookups)
	}
}

func TestSelectorLooksSecretMatchesWholeWords(t *testing.T) {
	t.Parallel()
	for _, sel := range []string{"#password", "input[type=password]", "#login-pw", "#passcode", "#passphrase", "#pin_code", "#pinCode",
		"#apiKey", "#api-key", "#api_key", "#access_key", "#cvv", "#security_code", "#verification_code", "#otp", "#mfa-token", "[name=secret]"} {
		if !selectorLooksSecret(sel) {
			t.Errorf("selectorLooksSecret(%q) = false, want true", sel)
		}
	}
	for _, sel := range []string{"#slotPicker", "#spotPrice", "#footprint", "#robotPolicy", "#bypass", "#compass", "#secretary", "#spin", "#q", "button:has-text(\"Log in\")"} {
		if selectorLooksSecret(sel) {
			t.Errorf("selectorLooksSecret(%q) = true, want false", sel)
		}
	}
}

// TestBrowserApprovalSummaryEdgeCases: an address carrying a plain credential
// is hidden; "Goes to" shows only when connector_studio will go there (not
// when the first step is itself a goto); steps without a session say so.
func TestBrowserApprovalSummaryEdgeCases(t *testing.T) {
	t.Parallel()
	rows := summaryRows(t, summarizeApprovalInput(tools.ConnectorStudioToolName,
		`{"action":"probe_browser","session_id":"s1","url":"https://a.example.com/?pw=hunter2x","password_ref":"hunter2x","steps":[{"action":"click","selector":"#go"}]}`, ""))
	if rows[2] != [2]string{"Goes to", "an address hidden here (it contains a credential)"} {
		t.Errorf("rows = %q, want the address hidden", rows)
	}
	rows = summaryRows(t, summarizeApprovalInput(tools.ConnectorStudioToolName,
		`{"action":"probe_browser","session_id":"s1","url":"https://a.example.com/","steps":[{"action":"goto","value":"https://a.example.com/x"}]}`, ""))
	for _, r := range rows {
		if r[0] == "Goes to" {
			t.Errorf("rows = %q, want no Goes to when the first step is a goto", rows)
		}
	}
	rows = summaryRows(t, summarizeApprovalInput(tools.PlaywrightBrowserToolName, `{"action":"steps","steps":[{"action":"click","selector":"#go"}]}`, ""))
	if rows[1] != [2]string{"Live session", "none given: this call will fail"} {
		t.Errorf("rows = %q, want the missing session named", rows)
	}
}

// TestBrowserApprovalSummaryWaitsMatchTheRunner: a sleep or an empty wait_for
// shows the pause the browser workers take (timeout_ms clamped to 500..10000,
// 10000 when omitted), and a wait_for on a value says what it waits for.
func TestBrowserApprovalSummaryWaitsMatchTheRunner(t *testing.T) {
	t.Parallel()
	rows := summaryRows(t, summarizeApprovalInput(tools.PlaywrightBrowserToolName, `{"action":"run","url":"https://a.example.com/","steps":[`+
		`{"action":"sleep"},{"action":"sleep","timeout_ms":100},{"action":"sleep","timeout_ms":25000},{"action":"wait_for","timeout_ms":3000},`+
		`{"action":"wait_for","value":"networkidle"},{"action":"wait_for","value":"https://a.example.com/done"},{"action":"click","selector":"#go"}]}`, ""))
	want := []string{"Wait 10000 ms", "Wait 500 ms", "Wait 10000 ms", "Wait 3000 ms", "Wait until the page stops loading", "Wait until the address is https://a.example.com/done"}
	for i, w := range want {
		if got := rows[2+i][1]; got != w {
			t.Errorf("step %d = %q, want %q", i+1, got, w)
		}
	}
}
