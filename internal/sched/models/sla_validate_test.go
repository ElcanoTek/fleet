package models

import (
	"strings"
	"testing"
)

// TestValidateSLA pins the create-time SLA consistency check (#274): nil
// expected duration is always valid (no SLA), a non-positive duration is
// rejected, negative multipliers are rejected, and a fail threshold at or
// below the warn threshold is rejected — including when only one side is
// explicit and the other resolves to its default.
func TestValidateSLA(t *testing.T) {
	cases := []struct {
		name     string
		expected *int
		warn     float64
		fail     float64
		wantErr  string // empty = valid
	}{
		{name: "nil expected is always valid", expected: nil, warn: -5, fail: -5},
		{name: "zero expected rejected", expected: slaIntPtr(0), wantErr: "expected_duration_minutes"},
		{name: "negative expected rejected", expected: slaIntPtr(-30), wantErr: "expected_duration_minutes"},
		{name: "defaults are consistent", expected: slaIntPtr(30)},
		{name: "explicit valid pair", expected: slaIntPtr(30), warn: 1.2, fail: 3},
		{name: "negative warn rejected", expected: slaIntPtr(30), warn: -1, fail: 2, wantErr: "multipliers must be >= 0"},
		{name: "negative fail rejected", expected: slaIntPtr(30), warn: 1.5, fail: -2, wantErr: "multipliers must be >= 0"},
		{name: "fail equal to warn rejected", expected: slaIntPtr(30), warn: 2, fail: 2, wantErr: "must exceed"},
		{name: "fail below warn rejected", expected: slaIntPtr(30), warn: 2, fail: 1.5, wantErr: "must exceed"},
		{name: "explicit warn above default fail rejected", expected: slaIntPtr(30), warn: 2.5, wantErr: "must exceed"},
		{name: "explicit fail below default warn rejected", expected: slaIntPtr(30), fail: 1.2, wantErr: "must exceed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateSLA(c.expected, c.warn, c.fail)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// TestResolveSLAMultipliers pins the shared default-resolution rule used by
// NewTask and storage.UpdateEditableTask: non-positive values map to the
// defaults, explicit positives pass through untouched.
func TestResolveSLAMultipliers(t *testing.T) {
	if w, f := ResolveSLAMultipliers(0, 0); w != DefaultSLAWarnMultiplier || f != DefaultSLAFailMultiplier {
		t.Errorf("zero values should resolve to defaults, got %v/%v", w, f)
	}
	if w, f := ResolveSLAMultipliers(-1, -1); w != DefaultSLAWarnMultiplier || f != DefaultSLAFailMultiplier {
		t.Errorf("negative values should resolve to defaults, got %v/%v", w, f)
	}
	if w, f := ResolveSLAMultipliers(1.25, 4); w != 1.25 || f != 4 {
		t.Errorf("explicit values should pass through, got %v/%v", w, f)
	}
}
