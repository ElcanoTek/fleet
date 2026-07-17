package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestMCPFastIODownload_ReplacesBearerURLBeforeRedaction(t *testing.T) {
	const signed = "https://api.fast.io/current/workspace/123/storage/node/read/?token=top-secret-download-token"
	broker := &recordingBroker{text: "**Result:** success\n\n# download_url\n" + signed + "\n\n# web_url\nhttps://elcano.fast.io/preview/node\n"}
	tool := &mcpTool{
		serverName: mcpServerFastIO,
		tool:       mcp.Tool{Name: "download", Description: "download"},
		broker:     broker,
	}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc-fastio-download", Input: `{}`})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("Run returned tool error: %s", resp.Content)
	}
	if strings.Contains(resp.Content, signed) || strings.Contains(resp.Content, "top-secret-download-token") {
		t.Fatalf("tool response leaked signed URL: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "token=[REDACTED]") {
		t.Fatalf("signed URL was redacted instead of converted to a usable handle: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "fleet-download://") {
		t.Fatalf("tool response did not expose an opaque handle: %s", resp.Content)
	}
}
