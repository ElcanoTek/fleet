package models

import "testing"

func TestLoopConfig_ValidateExitCondition(t *testing.T) {
	valid := []string{"llm", "shell:make test", "regex:^DONE$", "regex:.*"}
	for _, c := range valid {
		if err := (&LoopConfig{ExitCondition: c}).ValidateExitCondition(); err != nil {
			t.Errorf("ValidateExitCondition(%q) = %v, want nil", c, err)
		}
	}
	invalid := []string{"", "foo", "shell:", "shell:   ", "regex:(", "LLM", "shell"}
	for _, c := range invalid {
		if err := (&LoopConfig{ExitCondition: c}).ValidateExitCondition(); err == nil {
			t.Errorf("ValidateExitCondition(%q) = nil, want error", c)
		}
	}
}
