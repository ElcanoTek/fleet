package models

import "testing"

func TestRetryPolicy_Validate(t *testing.T) {
	valid := []*RetryPolicy{
		nil,
		{},
		{Backoff: BackoffExponential, InitialDelaySeconds: 60, MaxDelaySeconds: 3600},
		{Backoff: BackoffFixed, InitialDelaySeconds: 30, MaxDelaySeconds: 30},
		{RetryOn: []string{FailureTransient, FailureTimeoutPlaceholder()}, NoRetryOn: []string{FailureCostCeiling}},
	}
	for i, rp := range valid {
		if err := rp.Validate(); err != nil {
			t.Errorf("valid[%d].Validate() = %v, want nil", i, err)
		}
	}

	invalid := []*RetryPolicy{
		{Backoff: "linear"},                             // bad backoff type
		{InitialDelaySeconds: -1},                       // negative
		{InitialDelaySeconds: 100, MaxDelaySeconds: 50}, // initial > max
		{RetryOn: []string{"bogus"}},                    // unknown class
		{NoRetryOn: []string{"nope"}},                   // unknown class
	}
	for i, rp := range invalid {
		if err := rp.Validate(); err == nil {
			t.Errorf("invalid[%d] (%+v).Validate() = nil, want error", i, rp)
		}
	}
}

// FailureTimeoutPlaceholder returns a known class for the valid-list test (avoids
// hardcoding a second class literal). Uses an existing recognized class.
func FailureTimeoutPlaceholder() string { return FailureContextBudget }

func TestRetryPolicy_ShouldRetryClass(t *testing.T) {
	// nil policy: transient retries, nothing else (legacy behavior).
	var nilp *RetryPolicy
	if !nilp.ShouldRetryClass(FailureTransient) {
		t.Error("nil policy must retry transient")
	}
	for _, c := range []string{FailureCostCeiling, FailureContextBudget, FailureTerminal} {
		if nilp.ShouldRetryClass(c) {
			t.Errorf("nil policy must NOT retry %q", c)
		}
	}

	// Explicit retry_on broadens; no_retry_on wins.
	p := &RetryPolicy{RetryOn: []string{FailureTransient, FailureContextBudget}, NoRetryOn: []string{FailureContextBudget}}
	if !p.ShouldRetryClass(FailureTransient) {
		t.Error("should retry transient (in retry_on)")
	}
	if p.ShouldRetryClass(FailureContextBudget) {
		t.Error("no_retry_on must win over retry_on for context_budget")
	}
	if p.ShouldRetryClass(FailureCostCeiling) {
		t.Error("cost_ceiling not in retry_on → no retry")
	}

	// retry_on present but empty → nothing retries.
	empty := &RetryPolicy{RetryOn: []string{}}
	if empty.ShouldRetryClass(FailureTransient) {
		t.Error("empty retry_on must retry nothing")
	}
}

func TestRetryPolicy_EffectiveBackoff(t *testing.T) {
	// nil → legacy defaults.
	var nilp *RetryPolicy
	initSec, maxSec, exp := nilp.EffectiveBackoff()
	if initSec != DefaultRetryInitialDelaySeconds || maxSec != DefaultRetryMaxDelaySeconds || !exp {
		t.Errorf("nil EffectiveBackoff = (%d,%d,%v), want (30,600,true)", initSec, maxSec, exp)
	}

	// fixed + custom delays.
	initSec, maxSec, exp = (&RetryPolicy{Backoff: BackoffFixed, InitialDelaySeconds: 120, MaxDelaySeconds: 240}).EffectiveBackoff()
	if initSec != 120 || maxSec != 240 || exp {
		t.Errorf("custom fixed = (%d,%d,%v), want (120,240,false)", initSec, maxSec, exp)
	}

	// zero fields fall back to defaults.
	initSec, maxSec, exp = (&RetryPolicy{}).EffectiveBackoff()
	if initSec != 30 || maxSec != 600 || !exp {
		t.Errorf("empty policy = (%d,%d,%v), want defaults", initSec, maxSec, exp)
	}
}
