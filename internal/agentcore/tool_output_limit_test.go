package agentcore

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestApplyOutputCeiling(t *testing.T) {
	// Under the limit → unchanged.
	if out, trunc := applyOutputCeiling("short", 100); trunc || out != "short" {
		t.Errorf("under limit: trunc=%v out=%q, want false/short", trunc, out)
	}
	// Disabled (limit<=0) → unchanged.
	big := strings.Repeat("x", 10_000)
	if out, trunc := applyOutputCeiling(big, 0); trunc || out != big {
		t.Errorf("limit 0 must disable truncation")
	}

	// Over the limit → truncated, with head + tail preserved and a marker.
	content := strings.Repeat("A", 5_000) + "MIDDLE_NEEDLE" + strings.Repeat("B", 5_000)
	out, trunc := applyOutputCeiling(content, 2_000)
	if !trunc {
		t.Fatal("expected truncation over the limit")
	}
	if strings.Contains(out, "MIDDLE_NEEDLE") {
		t.Error("the middle should have been dropped")
	}
	if !strings.HasPrefix(out, "AAAA") || !strings.HasSuffix(out, "BBBB") {
		t.Errorf("head and tail must be preserved; got prefix %q suffix %q", out[:8], out[len(out)-8:])
	}
	if !strings.Contains(out, "truncated") {
		t.Error("a truncation marker should be present")
	}
}

// TestApplyOutputCeiling_UTF8Safe ensures cuts land on rune boundaries so the
// result stays valid UTF-8 (a mid-rune cut would corrupt the marshalled JSON).
func TestApplyOutputCeiling_UTF8Safe(t *testing.T) {
	// Multi-byte runes (™ is 3 bytes) packed so naive byte cuts land mid-rune.
	content := strings.Repeat("™", 4_000) // 12_000 bytes
	out, trunc := applyOutputCeiling(content, 3_000)
	if !trunc {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(out) {
		t.Error("truncated output must remain valid UTF-8")
	}
}

func TestMaxToolOutputBytes_Default(t *testing.T) {
	// No env override → the default (the once-cache reads env at first call; in a
	// clean test process FLEET_MAX_TOOL_OUTPUT_BYTES is unset).
	if got := maxToolOutputBytes(); got != defaultMaxToolOutputBytes {
		t.Errorf("maxToolOutputBytes() = %d, want default %d", got, defaultMaxToolOutputBytes)
	}
}
