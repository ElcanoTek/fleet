package models

import "testing"

func TestWorktreeConfig_Validate(t *testing.T) {
	// nil and disabled configs are always valid (no isolation requested).
	if err := (*WorktreeConfig)(nil).Validate(); err != nil {
		t.Errorf("nil.Validate() = %v, want nil", err)
	}
	if err := (&WorktreeConfig{Enabled: false, BranchPrefix: "bad prefix"}).Validate(); err != nil {
		t.Errorf("disabled config should skip validation, got %v", err)
	}

	valid := []*WorktreeConfig{
		{Enabled: true},
		{Enabled: true, BranchPrefix: "fleet/task-"},
		{Enabled: true, BranchPrefix: "iso/", BaseBranch: "main", AutoCleanup: true, CleanupDelaySeconds: 3600},
	}
	for i, wc := range valid {
		if err := wc.Validate(); err != nil {
			t.Errorf("valid[%d].Validate() = %v, want nil", i, err)
		}
	}

	invalid := []*WorktreeConfig{
		{Enabled: true, CleanupDelaySeconds: -1},     // negative delay
		{Enabled: true, BranchPrefix: "has space"},   // space not allowed in ref
		{Enabled: true, BranchPrefix: "tilde~"},      // ~ not allowed
		{Enabled: true, BranchPrefix: "caret^"},      // ^ not allowed
		{Enabled: true, BranchPrefix: "colon:"},      // : not allowed
		{Enabled: true, BranchPrefix: "q?"},          // ? not allowed
		{Enabled: true, BranchPrefix: "star*"},       // * not allowed
		{Enabled: true, BranchPrefix: "brk["},        // [ not allowed
		{Enabled: true, BranchPrefix: "back\\slash"}, // \ not allowed
		{Enabled: true, BranchPrefix: "at@{seq"},     // @{ sequence not allowed
		{Enabled: true, BranchPrefix: "a..b"},        // ".." not allowed in a ref
		{Enabled: true, BranchPrefix: "a//b"},        // "//" not allowed in a ref
		{Enabled: true, BranchPrefix: "a.lock/"},     // ".lock" component not allowed
	}
	for i, wc := range invalid {
		if err := wc.Validate(); err == nil {
			t.Errorf("invalid[%d] (%+v).Validate() = nil, want error", i, wc)
		}
	}
}
