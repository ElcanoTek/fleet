package scheduledrun

import (
	"context"
	"strings"
	"testing"
)

// The shared-file-library announcement (#1301) is best-effort like every
// other prompt section: nil provider (feature off / `fleet task run`) and an
// empty library both leave the prompt byte-for-byte untouched; a non-empty
// block is appended as its own section.
func TestAppendSharedFilesSection(t *testing.T) {
	ctx := context.Background()

	r := &Runner{}
	if got := r.appendSharedFilesSection(ctx, "base"); got != "base" {
		t.Fatalf("nil provider: prompt = %q, want untouched", got)
	}

	r = &Runner{sharedFilesPrompt: func(context.Context) string { return "" }}
	if got := r.appendSharedFilesSection(ctx, "base"); got != "base" {
		t.Fatalf("empty library: prompt = %q, want untouched", got)
	}

	block := "---\n**Shared file library** (…):\n- `shared/a.csv` (8 B)\n"
	r = &Runner{sharedFilesPrompt: func(context.Context) string { return block }}
	got := r.appendSharedFilesSection(ctx, "base")
	if !strings.HasPrefix(got, "base\n\n") || !strings.Contains(got, "`shared/a.csv`") {
		t.Fatalf("announced prompt = %q", got)
	}
}
