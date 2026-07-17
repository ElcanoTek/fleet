// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package clientconfig

import "testing"

func TestValidateHooks(t *testing.T) {
	good := &HooksConfig{Version: 1, Entries: []HookDef{
		{ID: "audit", Event: HookEventPreToolUse, Matcher: "bash", Command: "cat"},
		{ID: "fmt", Event: HookEventPostToolUse, Matcher: "edit_file", Command: "gofmt", TimeoutSeconds: 10},
		{ID: "greet", Event: HookEventUserPromptSubmit, Command: "cat"},
		{ID: "done", Event: HookEventTurnEnd, Command: "cat"},
	}}
	if err := validateHooks(good); err != nil {
		t.Fatalf("valid hooks rejected: %v", err)
	}

	// Empty/absent is valid (zero hooks).
	if err := validateHooks(nil); err != nil {
		t.Errorf("nil hooks: %v", err)
	}
	if err := validateHooks(&HooksConfig{}); err != nil {
		t.Errorf("empty hooks: %v", err)
	}

	cases := []struct {
		name string
		cfg  *HooksConfig
		want string
	}{
		{"bad version", &HooksConfig{Version: 2, Entries: []HookDef{{ID: "a", Event: HookEventTurnEnd, Command: "c"}}}, "version must be 1"},
		{"missing id", &HooksConfig{Version: 1, Entries: []HookDef{{Event: HookEventTurnEnd, Command: "c"}}}, "id is required"},
		{"dup id", &HooksConfig{Version: 1, Entries: []HookDef{
			{ID: "x", Event: HookEventTurnEnd, Command: "c"},
			{ID: "x", Event: HookEventUserPromptSubmit, Command: "c"},
		}}, "duplicate id"},
		{"unknown event", &HooksConfig{Version: 1, Entries: []HookDef{{ID: "x", Event: "nope", Command: "c"}}}, "unknown event"},
		{"empty command", &HooksConfig{Version: 1, Entries: []HookDef{{ID: "x", Event: HookEventTurnEnd, Command: "  "}}}, "command is required"},
		{"matcher on prompt", &HooksConfig{Version: 1, Entries: []HookDef{{ID: "x", Event: HookEventUserPromptSubmit, Matcher: "bash", Command: "c"}}}, "matcher is only valid"},
		{"timeout range", &HooksConfig{Version: 1, Entries: []HookDef{{ID: "x", Event: HookEventTurnEnd, Command: "c", TimeoutSeconds: 999}}}, "out of range"},
		{"exact dup", &HooksConfig{Version: 1, Entries: []HookDef{
			{ID: "a", Event: HookEventPreToolUse, Matcher: "bash", Command: "c"},
			{ID: "b", Event: HookEventPreToolUse, Matcher: "bash", Command: "c"},
		}}, "exact-duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHooks(tc.cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestEffectiveTimeoutSeconds(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 30}, {5, 5}, {200, 120}, {-3, 30}} {
		if got := (HookDef{TimeoutSeconds: tc.in}).EffectiveTimeoutSeconds(); got != tc.want {
			t.Errorf("timeout %d → %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestBundleHooksAccessor(t *testing.T) {
	b := &Bundle{HooksConfig: &HooksConfig{Version: 1, Entries: []HookDef{{ID: "a", Event: HookEventTurnEnd, Command: "c"}}}}
	hs := b.Hooks()
	if len(hs) != 1 || hs[0].ID != "a" {
		t.Fatalf("Hooks() = %+v", hs)
	}
	// Copy, not shared: mutating the returned slice must not affect the bundle.
	hs[0].ID = "mutated"
	if b.HooksConfig.Entries[0].ID != "a" {
		t.Error("Hooks() returned a shared reference, not a copy")
	}
	if (&Bundle{}).Hooks() != nil {
		t.Error("nil hooks bundle should return nil")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
