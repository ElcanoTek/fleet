package models

import (
	"strings"
	"testing"
)

func TestIsValidReportedStatus_DropsLegacyAnalyzing(t *testing.T) {
	if TaskStatusAnalyzing.IsValidReportedStatus() {
		t.Fatal("analyzing is a leftover moc status; workers must not report it")
	}
	for _, s := range []TaskStatus{TaskStatusLeased, TaskStatusRunning, TaskStatusSuccess, TaskStatusError} {
		if !s.IsValidReportedStatus() {
			t.Errorf("%q must remain worker-reportable", s)
		}
	}
}

func TestBudgetCreateValidate_RejectsProjectScope(t *testing.T) {
	hard := 10.0
	bc := BudgetCreate{
		Scope:       BudgetScopeProject,
		PrincipalID: "proj-1",
		Window:      BudgetWindowDay,
		HardUSD:     &hard,
	}
	err := bc.Validate()
	if err == nil {
		t.Fatal("scope=project must be rejected: tasks have no project dimension")
	}
	if !strings.Contains(err.Error(), "user|key") {
		t.Errorf("error should name the legal scopes, got %q", err)
	}

	bc.Scope = BudgetScopeUser
	if err := bc.Validate(); err != nil {
		t.Fatalf("scope=user must still validate: %v", err)
	}
}
