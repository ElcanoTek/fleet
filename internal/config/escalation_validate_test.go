package config

import (
	"strings"
	"testing"
)

// A mistyped classifier mode or model fails boot loudly (Validate calls this)
// rather than quietly turning the default-on check off or failing it on every
// message.
func TestValidateEscalationClassifier(t *testing.T) {
	for _, c := range []struct {
		mode, model string
		wantErr     string
	}{
		{"", "", ""},
		{"suggest", "~typesafe/jev-latest", ""},
		{"Observe", "liquid/d1", ""},
		{"off", "", ""},
		{"sugest", "", "FLEET_ESCALATION_CLASSIFIER_MODE"},
		{"suggest", "jev-latest", "FLEET_ESCALATION_CLASSIFIER_MODEL"},
		{"suggest", "typesafe/", "FLEET_ESCALATION_CLASSIFIER_MODEL"},
		{"suggest", "typesafe/jev latest", "FLEET_ESCALATION_CLASSIFIER_MODEL"},
	} {
		cfg := &Config{EscalationClassifierMode: c.mode, EscalationClassifierModel: c.model}
		err := cfg.validateEscalationClassifier()
		if c.wantErr == "" && err != nil {
			t.Errorf("mode %q model %q: unexpected error %v", c.mode, c.model, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("mode %q model %q: err = %v, want one naming %s", c.mode, c.model, err, c.wantErr)
		}
	}
}

// A value that validates is stored normalized, so the live read sees the mode
// the operator meant: " Suggest " must not pass boot and then read as off.
func TestValidateEscalationClassifier_Normalizes(t *testing.T) {
	cfg := &Config{EscalationClassifierMode: " Suggest ", EscalationClassifierModel: " liquid/d1 ", EscalationClassifierThreshold: 80}
	if err := cfg.validateEscalationClassifier(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	mode, model, _ := cfg.LiveEscalationClassifier()
	if mode != EscalationClassifierSuggest || model != "liquid/d1" {
		t.Fatalf("live = %q, %q; want suggest, liquid/d1", mode, model)
	}

	empty := &Config{}
	if err := empty.validateEscalationClassifier(); err != nil {
		t.Fatalf("validate empty: %v", err)
	}
	if mode, model, _ := empty.LiveEscalationClassifier(); mode != EscalationClassifierSuggest || model != DefaultEscalationClassifierModel {
		t.Fatalf("empty live = %q, %q; want the defaults", mode, model)
	}
}
