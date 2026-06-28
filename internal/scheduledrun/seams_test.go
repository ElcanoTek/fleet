package scheduledrun

import (
	"context"
	"testing"
)

// TestWorkspaceReporterRoundTrip verifies the context seam: an installed reporter
// receives the path, an empty path is never reported, and a context with no
// reporter is a safe no-op.
func TestWorkspaceReporterRoundTrip(t *testing.T) {
	t.Run("installed reporter receives path", func(t *testing.T) {
		var got string
		ctx := WithWorkspaceReporter(context.Background(), func(_ context.Context, p string) {
			got = p
		})
		reportWorkspacePath(ctx, "/var/lib/fleet/workspace/run-1")
		if got != "/var/lib/fleet/workspace/run-1" {
			t.Fatalf("reporter got %q; want the reported path", got)
		}
	})

	t.Run("empty path is never reported", func(t *testing.T) {
		called := false
		ctx := WithWorkspaceReporter(context.Background(), func(_ context.Context, _ string) {
			called = true
		})
		reportWorkspacePath(ctx, "")
		if called {
			t.Fatal("reporter must not fire for an empty path")
		}
	})

	t.Run("no reporter installed is a no-op", func(_ *testing.T) {
		// Must not panic.
		reportWorkspacePath(context.Background(), "/some/path")
	})

	t.Run("nil reporter leaves context untouched", func(_ *testing.T) {
		ctx := WithWorkspaceReporter(context.Background(), nil)
		// Still a no-op, no panic.
		reportWorkspacePath(ctx, "/some/path")
	})
}
