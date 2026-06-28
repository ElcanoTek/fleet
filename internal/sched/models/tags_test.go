package models

import (
	"strings"
	"testing"
)

func TestNormalizeAndValidateTags(t *testing.T) {
	t.Run("normalizes case, trims, dedupes, preserves order", func(t *testing.T) {
		got, err := NormalizeAndValidateTags([]string{" Nightly ", "PROD", "nightly", "", "  "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"nightly", "prod"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("empty/all-blank → nil", func(t *testing.T) {
		if got, err := NormalizeAndValidateTags(nil); err != nil || got != nil {
			t.Errorf("nil → (%v, %v), want (nil, nil)", got, err)
		}
		if got, err := NormalizeAndValidateTags([]string{"", "   "}); err != nil || got != nil {
			t.Errorf("all-blank → (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("allows hyphen and dot", func(t *testing.T) {
		if _, err := NormalizeAndValidateTags([]string{"data-pipeline", "v1.2"}); err != nil {
			t.Errorf("hyphen/dot should be allowed, got %v", err)
		}
	})

	t.Run("rejects invalid characters", func(t *testing.T) {
		for _, bad := range []string{"has space", "UPPER", "under_score", "slash/x", "emoji😀", "comma,x"} {
			// UPPER is lowercased first, so test the raw lowercasing-then-validate path
			// with genuinely invalid chars (space/_/slash/emoji/comma).
			if bad == "UPPER" {
				continue
			}
			if _, err := NormalizeAndValidateTags([]string{bad}); err == nil {
				t.Errorf("tag %q should be rejected", bad)
			}
		}
	})

	t.Run("rejects over-long tag", func(t *testing.T) {
		if _, err := NormalizeAndValidateTags([]string{strings.Repeat("a", MaxTagLength+1)}); err == nil {
			t.Error("tag over MaxTagLength should be rejected")
		}
		if _, err := NormalizeAndValidateTags([]string{strings.Repeat("a", MaxTagLength)}); err != nil {
			t.Errorf("tag at MaxTagLength should be accepted, got %v", err)
		}
	})

	t.Run("rejects too many tags", func(t *testing.T) {
		many := make([]string, MaxTagsPerTask+1)
		for i := range many {
			many[i] = "tag" + strings.Repeat("x", i%3) + string(rune('a'+i)) // distinct
		}
		if _, err := NormalizeAndValidateTags(many); err == nil {
			t.Error("more than MaxTagsPerTask distinct tags should be rejected")
		}
	})
}
