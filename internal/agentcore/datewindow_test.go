package agentcore

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

func mustUTC(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return ts
}

// Date rollover (#1026): a chat that searched August 12 must, on the next
// UTC day, resolve runtime_today and freshness_window through August 13
// without reading yesterday's bounds.
func TestRuntimeToday_DateRollover(t *testing.T) {
	aug12 := mustUTC(t, "2026-08-12T22:00:00Z")
	aug13 := mustUTC(t, "2026-08-13T00:00:01Z")

	if got := RuntimeToday(aug12); got != "2026-08-12" {
		t.Fatalf("aug12 runtime_today=%s, want 2026-08-12", got)
	}
	if got := RuntimeToday(aug13); got != "2026-08-13" {
		t.Fatalf("aug13 runtime_today=%s, want 2026-08-13", got)
	}

	from12, to12 := FreshnessWindow(aug12)
	if from12 != "2026-08-10" || to12 != "2026-08-12" {
		t.Fatalf("aug12 freshness=%s..%s, want 2026-08-10..2026-08-12", from12, to12)
	}
	from13, to13 := FreshnessWindow(aug13)
	if from13 != "2026-08-11" || to13 != "2026-08-13" {
		t.Fatalf("aug13 freshness=%s..%s, want 2026-08-11..2026-08-13", from13, to13)
	}

	suffix := RuntimeDateTurnSuffix(aug13)
	if !strings.Contains(suffix, "runtime_today: 2026-08-13") {
		t.Fatalf("suffix missing runtime_today:\n%s", suffix)
	}
	if !strings.Contains(suffix, "2026-08-11 .. 2026-08-13") {
		t.Fatalf("suffix missing freshness_window through the 13th:\n%s", suffix)
	}
	if strings.Contains(suffix, "2026-08-12 .. 2026-08-12") {
		t.Fatal("suffix still anchored to the prior day's single-day window")
	}
}

func TestRuntimeDateTurnSuffix_StableWithinUTCDay(t *testing.T) {
	day := mustUTC(t, "2026-08-13T00:00:00Z")
	want := RuntimeDateTurnSuffix(day)
	for _, offset := range []time.Duration{
		time.Second,
		5 * time.Hour,
		23*time.Hour + 59*time.Minute,
	} {
		got := RuntimeDateTurnSuffix(day.Add(offset))
		if got != want {
			t.Fatalf("suffix changed %s into the UTC day", offset)
		}
	}
	if RuntimeDateTurnSuffix(day.Add(24*time.Hour)) == want {
		t.Fatal("suffix must change across the UTC day boundary")
	}
}

func TestAppendRuntimeDateMessage_DoesNotRewriteHistory(t *testing.T) {
	prior := []fantasy.Message{fantasy.NewUserMessage("check again for the latest report")}
	out := appendRuntimeDateMessage(prior, mustUTC(t, "2026-08-13T15:00:00Z"))
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2 (original + suffix)", len(out))
	}
	if got := msgText(out[0]); got != "check again for the latest report" {
		t.Fatalf("rewrote the user turn: %q", got)
	}
	if !strings.Contains(msgText(out[1]), "runtime_today: 2026-08-13") {
		t.Fatalf("missing suffix on trailing message: %q", msgText(out[1]))
	}
}

// Stale date_to on a mailbox search is annotated so the model cannot treat
// a yesterday-only window as a complete "latest" check.
func TestAnnotateDateWindow_StaleDateTo(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	input := `{"sender_contains":"reports@openx.com","subject_contains":"OpenX | Daily","date_from":"2026-08-12","date_to":"2026-08-12","has_payload":true}`
	result := `{"status":"success","matches_found":0,"emails":[]}`
	got := AnnotateDateWindow("mcp_email_search_emails", input, result, now)
	if !strings.Contains(got, "[fleet date-window]") {
		t.Fatalf("missing stale-window note:\n%s", got)
	}
	if !strings.Contains(got, "date_to=2026-08-12") {
		t.Fatalf("note should name the stale date_to:\n%s", got)
	}
	if !strings.Contains(got, "runtime_today=2026-08-13") {
		t.Fatalf("note should name runtime_today:\n%s", got)
	}
	if !strings.Contains(got, "freshness_window=2026-08-11..2026-08-13") {
		t.Fatalf("note should name the freshness window:\n%s", got)
	}
}

func TestAnnotateDateWindow_OnOrBeforeAlias(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	input := `{"sender_contains":"openx.com","on_or_before":"2026-08-12","lookback_days":3}`
	got := AnnotateDateWindow("mcp_email_find_latest_report", input, `{"status":"success"}`, now)
	if !strings.Contains(got, "on_or_before=2026-08-12") {
		t.Fatalf("should treat on_or_before as an upper bound:\n%s", got)
	}
}

func TestAnnotateDateWindow_CurrentWindowUnchanged(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	input := `{"date_from":"2026-08-11","date_to":"2026-08-13"}`
	result := `{"status":"success","matches_found":2,"emails":[{"email_id":"a"},{"email_id":"b"}]}`
	got := AnnotateDateWindow("mcp_email_search_emails", input, result, now)
	if got != result {
		t.Fatalf("current window + hits should be unchanged:\n%s", got)
	}
}

func TestAnnotateDateWindow_HistoricalWindowWithHitsUnchanged(t *testing.T) {
	// A deliberate historical query that found mail is not a stale-"latest"
	// failure. We still flag the stale upper bound (the model may have meant
	// "latest") but do NOT add the empty-search fallback.
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	input := `{"date_from":"2026-08-01","date_to":"2026-08-01"}`
	result := `{"status":"success","matches_found":1,"emails":[{"email_id":"x"}]}`
	got := AnnotateDateWindow("mcp_email_search_emails", input, result, now)
	if !strings.Contains(got, "[fleet date-window]") {
		t.Fatal("historical date_to still excludes today and should be flagged")
	}
	if strings.Contains(got, "[fleet search]") {
		t.Fatal("non-empty historical result must not get the empty-search fallback")
	}
}

// Exact-search false negative (#1026): an empty exact sender/subject
// result is annotated so the workflow cannot treat it as absence.
func TestAnnotateDateWindow_EmptyExactSearchIsNotAbsence(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	input := `{"sender_contains":"reports@openx.com","subject_contains":"OpenX | Elcano","date_from":"2026-08-13","date_to":"2026-08-13","has_payload":true}`
	result := `{"status":"success","matches_found":0,"emails":[]}`
	got := AnnotateDateWindow("mcp_email_search_emails", input, result, now)
	if strings.Contains(got, "[fleet date-window]") {
		t.Fatal("window already includes today; should not flag date_to")
	}
	if !strings.Contains(got, "[fleet search]") {
		t.Fatalf("empty exact search must be flagged as non-authoritative:\n%s", got)
	}
	if !strings.Contains(got, "recipient") || !strings.Contains(got, "fuzzy") {
		t.Fatalf("fallback reminder should name recipient/fuzzy paths:\n%s", got)
	}
}

func TestAnnotateDateWindow_SubjectPunctuationStillParsed(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	// The pipe in an OpenX subject must not prevent JSON/date parsing.
	input := `{"sender_contains":"reports@openx.com","subject_contains":"OpenX | Daily Performance | Elcano","date_from":"2026-08-12","date_to":"2026-08-12","has_payload":true}`
	result := `{"status":"success","matches_found":0,"emails":[]}`
	got := AnnotateDateWindow("mcp_email_search_emails", input, result, now)
	if !strings.Contains(got, "[fleet date-window]") || !strings.Contains(got, "[fleet search]") {
		t.Fatalf("subject punctuation must not drop annotations:\n%s", got)
	}
}

func TestAnnotateDateWindow_IgnoresUnrelatedTools(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	input := `{"path":"foo.go"}`
	result := `{"ok":true}`
	got := AnnotateDateWindow("view_file", input, result, now)
	if got != result {
		t.Fatalf("non-search tool should be unchanged: %s", got)
	}
}

func TestAnnotateDateWindow_RFC3339DateTo(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	input := `{"date_to":"2026-08-12T23:59:59Z"}`
	got := AnnotateDateWindow("mcp_email_search_emails", input, `{"matches_found":1,"emails":[{}]}`, now)
	if !strings.Contains(got, "date_to=2026-08-12") {
		t.Fatalf("RFC3339 date_to should collapse to the UTC day:\n%s", got)
	}
}

func TestParseDateBound(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"2026-08-12", "2026-08-12", true},
		{"2026-08-12T15:03:00Z", "2026-08-12", true},
		{"2026-08-13T00:00:00+00:00", "2026-08-13", true},
		{"2026-08-12 15:03:00", "2026-08-12", true},
		{"", "", false},
		{"not-a-date", "", false},
	}
	for _, tc := range cases {
		got, ok := parseDateBound(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseDateBound(%q)=(%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRun_InjectsRuntimeToday(t *testing.T) {
	now := mustUTC(t, "2026-08-13T09:00:00Z")
	orig := runtimeNow
	runtimeNow = func() time.Time { return now }
	t.Cleanup(func() { runtimeNow = orig })

	session := NewLogSession()
	model := &capturingModel{slug: "date-window-model", inputByCall: []int{10}}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:      historyInput{system: "s", msgs: []fantasy.Message{fantasy.NewUserMessage("check again for the latest report")}, label: "date"},
		Policy:     newRoundsPolicy(session, 0),
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawToday, sawPriorDay bool
	for _, m := range model.call(0) {
		text := msgText(m)
		if strings.Contains(text, "runtime_today: 2026-08-13") {
			sawToday = true
		}
		if strings.Contains(text, "runtime_today: 2026-08-12") {
			sawPriorDay = true
		}
	}
	if !sawToday {
		t.Fatal("Run must inject runtime_today for the current UTC date into the model input")
	}
	if sawPriorDay {
		t.Fatal("Run must not inject the previous day's runtime_today")
	}
}

func TestMCPTool_AnnotatesStaleDateWindow(t *testing.T) {
	now := mustUTC(t, "2026-08-13T16:00:00Z")
	orig := runtimeNow
	runtimeNow = func() time.Time { return now }
	t.Cleanup(func() { runtimeNow = orig })

	broker := &recordingBroker{text: `{"status":"success","matches_found":0,"emails":[]}`}
	tool := &mcpTool{
		serverName: "email",
		tool:       mcp.Tool{Name: "search_emails", Description: "d"},
		broker:     broker,
	}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "tc-date",
		Input: `{"sender_contains":"reports@openx.com","subject_contains":"OpenX | Daily","date_from":"2026-08-12","date_to":"2026-08-12","has_payload":true}`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(resp.Content, "[fleet date-window]") {
		t.Fatalf("mcpTool result missing stale-window note:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "[fleet search]") {
		t.Fatalf("mcpTool result missing empty-search fallback:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, `"matches_found":0`) && !strings.Contains(resp.Content, `"matches_found": 0`) {
		t.Fatalf("original payload should still be present:\n%s", resp.Content)
	}
}
