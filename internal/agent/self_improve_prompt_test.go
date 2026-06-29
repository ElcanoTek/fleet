package agent

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/tools"
)

func TestAppendTaskMemorySection(t *testing.T) {
	// With memories: every fact is rendered and the tools are advertised.
	withMems := appendTaskMemorySection("BASE", []tools.TaskMemory{
		{Key: "last_seen_price", Value: "42.17"},
		{Key: "seen", Value: "[a, b]"},
	})
	if !strings.HasPrefix(withMems, "BASE") {
		t.Fatal("section must append after the base prompt")
	}
	for _, want := range []string{"Your Persistent Memory", "remember", "recall", "last_seen_price", "42.17", "seen"} {
		if !strings.Contains(withMems, want) {
			t.Errorf("memory section missing %q:\n%s", want, withMems)
		}
	}

	// No memories (first run): still advertises the capability, notes none saved.
	empty := appendTaskMemorySection("BASE", nil)
	if !strings.Contains(empty, "No facts saved yet") || !strings.Contains(empty, "remember") {
		t.Errorf("empty memory section should note 'no facts yet' but still advertise the tool:\n%s", empty)
	}
	if strings.Contains(empty, "42.17") {
		t.Error("empty section must not render any facts")
	}
}
