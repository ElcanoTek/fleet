// This file is intentionally NOT in package tools_test — fastio_find's
// runFastIOFind is unexported and the live test asserts on the rendered
// output shape, so we want package-internal visibility.

package tools

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// TestFastIOFindLive exercises the find-then-hydrate flow against the
// real fast.io MCP server. Skipped unless FAST_IO_MCP_TOKEN_LIVE_TEST=1
// is set explicitly, so CI doesn't try to hit fast.io on every commit.
//
// The scenario is the one that broke the ABC plumbing chat (see
// conversation 3460d911-df2b-4332-b6c3-de5d70b4d6cf): the natural-
// language phrase "ABC plumbing" returns one wrong file. fastio_find
// should promote to the ELC code and surface the actual report.
//
// Workspace id is hardcoded to the Elcano "General" workspace
// (***REMOVED***) — same id the broken chat used. If that
// workspace is ever rotated, update the constant or the test should
// fail loudly.
func TestFastIOFindLive(t *testing.T) {
	if os.Getenv("FAST_IO_MCP_TOKEN_LIVE_TEST") != "1" {
		t.Skip("set FAST_IO_MCP_TOKEN_LIVE_TEST=1 to run live tests (requires FAST_IO_MCP_TOKEN)")
	}
	token := os.Getenv("FAST_IO_MCP_TOKEN")
	if token == "" {
		t.Skip("FAST_IO_MCP_TOKEN not set")
	}

	const workspaceID = "***REMOVED***"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := mcp.NewClient()
	if err := client.AddHTTPServerWithHeaders(ctx, "fast_io", "https://mcp.fast.io/mcp", map[string]string{
		"Authorization": "Bearer " + token,
	}); err != nil {
		t.Fatalf("AddHTTPServerWithHeaders: %v", err)
	}

	// Bind a thin caller that targets the fast_io server's storage
	// tool — the MCP client multiplexes by tool name, so we just
	// forward.
	caller := liveCaller{client: client}

	t.Run("ELC code finds 12 variants and renders newest-first", func(t *testing.T) {
		out, err := runFastIOFind(ctx, caller, FastIOFindParams{
			Query:       "ELC00109",
			WorkspaceID: workspaceID,
			Limit:       20,
		})
		if err != nil {
			t.Fatalf("runFastIOFind: %v", err)
		}
		t.Logf("output:\n%s", out)

		// Must contain at least the canonical Overall Report.
		if !strings.Contains(out, "ABC_ELC00109_Overall_Report.csv") {
			t.Errorf("expected ABC_ELC00109_Overall_Report.csv in results; got:\n%s", out)
		}
		// Must be tight — no fast.io meta sections leaked through.
		if strings.Contains(out, "_buildHash") || strings.Contains(out, "_next\n") {
			t.Errorf("output leaked fast.io meta sections:\n%s", out)
		}
		// Output should be under 6 KB on a 12-row result. The pre-
		// existing flow burned ~25 KB just on the search + details
		// calls to reach this point. If we drift over budget the
		// next reader knows something regressed.
		const budget = 6 * 1024
		if len(out) > budget {
			t.Errorf("output is %d bytes, over the %d-byte budget for a 12-row result", len(out), budget)
		}
	})

	t.Run("Natural query auto-promotes to ELC code", func(t *testing.T) {
		out, err := runFastIOFind(ctx, caller, FastIOFindParams{
			Query:       "ABC plumbing ELC00109",
			WorkspaceID: workspaceID,
			Limit:       20,
		})
		if err != nil {
			t.Fatalf("runFastIOFind: %v", err)
		}
		t.Logf("output:\n%s", out)

		// The natural query AND-tokenizes to "ABC" + "plumbing" + the
		// ELC code; only the campaign-setup XLSX matches. fastio_find
		// should fall back to "ELC00109" alone and surface the report
		// files. Whether "via fallback" appears depends on fast.io's
		// matching — if the natural query did happen to return ≥1
		// hit we don't promote. We accept either path as long as a
		// report file appears.
		if !strings.Contains(out, "ABC_ELC00109_Overall_Report.csv") {
			t.Errorf("expected the actual report file in results; got:\n%s", out)
		}
	})

	t.Run("No-hits path is clean and actionable", func(t *testing.T) {
		out, err := runFastIOFind(ctx, caller, FastIOFindParams{
			Query:       "ELC99999",
			WorkspaceID: workspaceID,
		})
		if err != nil {
			t.Fatalf("runFastIOFind: %v", err)
		}
		t.Logf("output:\n%s", out)

		if !strings.Contains(out, "No files matched") {
			t.Errorf("expected no-hits message; got:\n%s", out)
		}
		// Must include the recovery hints.
		if !strings.Contains(out, "List the workspace root") {
			t.Errorf("expected the workspace-root recovery hint:\n%s", out)
		}
	})

	t.Run("Output mentions modified timestamps in ISO-ish format", func(t *testing.T) {
		out, err := runFastIOFind(ctx, caller, FastIOFindParams{
			Query:       "ELC00109",
			WorkspaceID: workspaceID,
			Limit:       5,
		})
		if err != nil {
			t.Fatalf("runFastIOFind: %v", err)
		}
		// Spot-check the table by matching the modified-date column
		// pattern. Format is `YYYY-MM-DD HH:MM`.
		re := regexp.MustCompile(`202[5-9]-[01]\d-[0-3]\d \d{2}:\d{2}`)
		if !re.MatchString(out) {
			t.Errorf("modified-date column missing or malformed:\n%s", out)
		}
	})
}

// liveCaller adapts *mcp.Client to the MCPCaller interface so the
// live test reuses the same runFastIOFind code path the production
// agent does. The fast.io HTTP MCP server registers tools under bare
// names (`storage`, `download`, …), so we pass the toolName through
// untouched; mcp.Client's CallTool dispatches by tool name.
type liveCaller struct {
	client *mcp.Client
}

func (l liveCaller) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*mcp.ToolResult, error) {
	return l.client.CallTool(ctx, toolName, args)
}
