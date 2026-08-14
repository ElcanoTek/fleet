package admincli

import "testing"

func TestSchedTaskSetLimits_UsageAndValidation(t *testing.T) {
	if code := schedTaskSetLimits(nil); code != 1 {
		t.Errorf("missing args: code=%d want 1", code)
	}
	if code := schedTaskSetLimits([]string{"not-a-uuid"}); code != 1 {
		t.Errorf("bad id: code=%d want 1", code)
	}
	id := "11111111-1111-1111-1111-111111111111"
	if code := schedTaskSetLimits([]string{id}); code != 1 {
		t.Errorf("no flags: code=%d want 1", code)
	}
	if code := schedTaskSetLimits([]string{id, "--memory-mb", "64"}); code != 1 {
		t.Errorf("below floor: code=%d want 1", code)
	}
	if code := schedTaskSetLimits([]string{id, "--clear", "--memory-mb", "256"}); code != 1 {
		t.Errorf("clear+set: code=%d want 1", code)
	}
}

func TestSchedBudgetCreate_Validation(t *testing.T) {
	if code := schedBudgetCreate([]string{"--scope", "project", "--principal", "p", "--hard-usd", "1"}); code != 1 {
		t.Errorf("scope=project: code=%d want 1", code)
	}
	if code := schedBudgetCreate([]string{"--principal", "", "--hard-usd", "1"}); code != 1 {
		t.Errorf("empty principal: code=%d want 1", code)
	}
	if code := schedBudgetCreate([]string{"--principal", "alice@example.com"}); code != 1 {
		t.Errorf("no bounds: code=%d want 1", code)
	}
}
