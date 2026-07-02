package httpapi

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

func testCatalog() []agent.OptionalServerInfo {
	return []agent.OptionalServerInfo{
		{Name: "slack", DisplayName: "Slack", Description: "send and read Slack messages and channels"},
		{Name: "jira", DisplayName: "Jira", Description: "track issues and tickets in Jira projects"},
		{Name: "gcal", DisplayName: "Google Calendar", Description: "read and create calendar events and meetings"},
	}
}

func TestRecommendConnectors_Match(t *testing.T) {
	recs := recommendConnectors("please post this update to our slack channel", testCatalog(), nil, 2)
	if len(recs) == 0 || recs[0].Name != "slack" {
		t.Fatalf("expected slack recommended first, got %+v", recs)
	}
	for _, r := range recs {
		if r.Name == "jira" {
			t.Errorf("unrelated connector jira should not be recommended: %+v", recs)
		}
	}
}

func TestRecommendConnectors_ExcludesEnabled(t *testing.T) {
	// slack is already enabled for this turn → never recommended.
	recs := recommendConnectors("post to slack", testCatalog(), map[string]bool{"slack": true}, 2)
	for _, r := range recs {
		if r.Name == "slack" {
			t.Errorf("an already-enabled connector must not be recommended: %+v", recs)
		}
	}
}

func TestRecommendConnectors_NoOverlapNoRecs(t *testing.T) {
	if recs := recommendConnectors("the weather is nice today", testCatalog(), nil, 2); len(recs) != 0 {
		t.Errorf("a message with no connector overlap should recommend nothing, got %+v", recs)
	}
}

func TestRecommendConnectors_EmptyInputs(t *testing.T) {
	if recs := recommendConnectors("", testCatalog(), nil, 2); recs != nil {
		t.Errorf("empty message → nil, got %+v", recs)
	}
	if recs := recommendConnectors("slack", nil, nil, 2); recs != nil {
		t.Errorf("empty catalog → nil, got %+v", recs)
	}
	if recs := recommendConnectors("slack", testCatalog(), nil, 0); recs != nil {
		t.Errorf("zero limit → nil, got %+v", recs)
	}
}

func TestRecommendConnectors_Cap(t *testing.T) {
	// All three mention "calendar/meeting/event"-ish? Use a query hitting several.
	catalog := []agent.OptionalServerInfo{
		{Name: "a", Description: "sync data to warehouse"},
		{Name: "b", Description: "export data to spreadsheet"},
		{Name: "c", Description: "stream data to dashboard"},
	}
	recs := recommendConnectors("move the data please", catalog, nil, 2)
	if len(recs) > 2 {
		t.Errorf("recommendations must respect the limit, got %d", len(recs))
	}
}

func TestAppendConnectorRecommendationBlock(t *testing.T) {
	if got := appendConnectorRecommendationBlock("hi", nil); got != "hi" {
		t.Errorf("no recs → no-op, got %q", got)
	}
	got := appendConnectorRecommendationBlock("base message", []agent.OptionalServerInfo{
		{Name: "slack", DisplayName: "Slack", Description: "send Slack messages"},
	})
	if !strings.Contains(got, "base message") {
		t.Error("original message dropped")
	}
	if !strings.Contains(got, "Slack") || !strings.Contains(got, "send Slack messages") {
		t.Errorf("connector not described: %q", got)
	}
	if !strings.Contains(got, "/settings/connections") {
		t.Errorf("block should link the connections page: %q", got)
	}
	if !strings.Contains(got, "do NOT claim you can use it") {
		t.Errorf("block should warn the model the tools aren't available yet: %q", got)
	}
}
