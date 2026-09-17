package agentcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestMCPWorkspaceFileChunks(t *testing.T) {
	// Deliberately bigger than a model response; includes UTF-8 crossing chunks.
	data := bytes.Repeat([]byte("Northwind ☂\n"), 25000)
	hash := sha256.Sum256(data)
	broker := &recordingBroker{text: `{"accepted":true}`}
	policy := &gatePolicy{}
	reads := 0
	tool := &mcpTool{serverName: "artifact_store", tool: mcp.Tool{Name: "append", InputSchema: map[string]any{"properties": map[string]any{
		"chunk": map[string]any{"type": "string", "contentEncoding": "base64", "maxLength": 65536},
	}}}, broker: broker, policy: policy, readWorkspaceFile: func(_ context.Context, path string, capBytes int64) ([]byte, error) {
		reads++
		if path != "report.json" || capBytes != maxMCPWorkspaceFileBytes {
			t.Fatal("wrong read contract")
		}
		return data, nil
	}}
	if !strings.Contains(mustFileJSON(t, tool.Info().Parameters), "workspace_file") {
		t.Fatal("reference not advertised")
	}
	var transferred []byte
	for offset, sequence := 0, 0; offset < len(data); sequence++ {
		length := min(49152, len(data)-offset)
		input := mustFileJSON(t, map[string]any{"sequence": sequence, "chunk": workspaceFileArgument{"report.json", hex.EncodeToString(hash[:]), offset, length}})
		result, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "append", Input: input})
		if err != nil || result.IsError || !policy.recordOK {
			t.Fatalf("append failed: %+v %v", result, err)
		}
		part, err := base64.StdEncoding.DecodeString(broker.gotArgs["chunk"].(string))
		if err != nil {
			t.Fatal(err)
		}
		transferred = append(transferred, part...)
		if strings.Contains(result.Content, broker.gotArgs["chunk"].(string)) {
			t.Fatal("file bytes returned to model")
		}
		offset += length
	}
	if !bytes.Equal(transferred, data) || reads != broker.calls {
		t.Fatal("file was corrupted or calls bypassed reader")
	}
	// Gate refusal happens before even reading the file, under the original MCP name.
	policy.block = true
	policy.blockMsg = "APPROVAL_REQUIRED"
	before := reads
	_, _ = tool.Run(context.Background(), fantasy.ToolCall{ID: "blocked", Input: mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"report.json", hex.EncodeToString(hash[:]), 0, 32}})})
	if reads != before || broker.calls != before {
		t.Fatal("approval bypassed")
	}
	policy.block = false
	for _, ref := range []any{
		workspaceFileArgument{"report.json", strings.Repeat("0", 64), 0, 32},
		workspaceFileArgument{"report.json", hex.EncodeToString(hash[:]), len(data), 1},
		workspaceFileArgument{"report.json", hex.EncodeToString(hash[:]), 0, 49153},
		map[string]any{"workspace_file": "report.json", "sha256": hex.EncodeToString(hash[:]), "offset": 0.5, "length": 32},
		map[string]any{"workspace_file": "report.json", "sha256": hex.EncodeToString(hash[:]), "length": 32},
	} {
		calls := broker.calls
		result, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "invalid", Input: mustFileJSON(t, map[string]any{"chunk": ref})})
		if err != nil || !result.IsError || broker.calls != calls || policy.recordOK {
			t.Fatalf("invalid reference dispatched: %+v", ref)
		}
	}
	// Without an opted-in annotation, the wrapper never interprets an object.
	delete(tool.tool.InputSchema["properties"].(map[string]any)["chunk"].(map[string]any), "contentEncoding")
	if strings.Contains(mustFileJSON(t, tool.Info().Parameters), "workspace_file") {
		t.Fatal("unannotated field advertised reference")
	}
}

func mustFileJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
