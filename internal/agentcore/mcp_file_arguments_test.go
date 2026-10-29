package agentcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/tools"
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
	}}}, broker: broker, policy: policy, readWorkspaceFile: fakeWorkspaceReader(t, "report.json", data, &reads)}
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

// fakeWorkspaceReader mirrors tools.WorkspaceFileReader's contract: it
// refuses a file over the cap, hashes the whole file itself and refuses a
// mismatch, and returns only the requested window.
func fakeWorkspaceReader(t *testing.T, name string, data []byte, reads *int) tools.WorkspaceFileReadFunc {
	t.Helper()
	sum := sha256.Sum256(data)
	return func(_ context.Context, refs []tools.WorkspaceFileRef, maxFileBytes int64) ([][]byte, func(), error) {
		*reads++
		windows := make([][]byte, len(refs))
		for i, ref := range refs {
			if ref.Path != name || maxFileBytes != maxMCPWorkspaceFileBytes {
				t.Fatalf("wrong read contract: %q %d", ref.Path, maxFileBytes)
			}
			size := int64(len(data))
			if size > maxFileBytes {
				return nil, nil, fmt.Errorf("file is %d bytes, exceeding the %d-byte cap", size, maxFileBytes)
			}
			if hex.EncodeToString(sum[:]) != ref.SHA256 {
				return nil, nil, tools.ErrWorkspaceFileHashMismatch
			}
			start, end := min(ref.Offset, size), min(ref.Offset+ref.Length, size)
			windows[i] = data[start:end]
		}
		return windows, func() {}, nil
	}
}

// TestMCPWorkspaceFileCap pins the whole-file cap at 64 MiB (ADR-0088): a file
// well past the old 2 MiB cap is accepted and its window delivered exactly, a
// file one byte over the cap is refused before any MCP call, and the
// advertised description and schema maxima carry the same number.
func TestMCPWorkspaceFileCap(t *testing.T) {
	if maxMCPWorkspaceFileBytes != 64<<20 {
		t.Fatalf("cap = %d, want 64 MiB", maxMCPWorkspaceFileBytes)
	}
	schema := map[string]any{"properties": map[string]any{
		"chunk": map[string]any{"type": "string", "contentEncoding": "base64"},
	}}
	newTool := func(data []byte, reads *int) (*mcpTool, *recordingBroker) {
		broker := &recordingBroker{text: `{"accepted":true}`}
		return &mcpTool{serverName: "pages", tool: mcp.Tool{Name: "upload_chunk", InputSchema: schema},
			broker: broker, policy: &gatePolicy{}, readWorkspaceFile: fakeWorkspaceReader(t, "site.zip", data, reads)}, broker
	}

	reads := 0
	tool, _ := newTool(nil, &reads)
	params := mustFileJSON(t, tool.Info().Parameters)
	if maxMCPWorkspaceWindowBytes != 8<<20 {
		t.Fatalf("per-call window = %d, want 8 MiB", maxMCPWorkspaceWindowBytes)
	}
	if !strings.Contains(params, fmt.Sprintf("Maximum whole file: %d bytes; maximum length per call: %d bytes.", maxMCPWorkspaceFileBytes, maxMCPWorkspaceWindowBytes)) ||
		!strings.Contains(params, fmt.Sprintf(`"maximum":%d`, maxMCPWorkspaceFileBytes-1)) ||
		!strings.Contains(params, fmt.Sprintf(`"maximum":%d`, maxMCPWorkspaceWindowBytes)) {
		t.Fatalf("advertised schema does not carry the cap: %s", params)
	}

	// 3 MiB: over the old cap. The last MiB is sent as one window.
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = byte(i * 31)
	}
	bigSum := sha256.Sum256(big)
	tool, broker := newTool(big, &reads)
	input := mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"site.zip", hex.EncodeToString(bigSum[:]), 2 << 20, 1 << 20}})
	result, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "big", Input: input})
	if err != nil || result.IsError || broker.calls != 1 {
		t.Fatalf(">2 MiB file refused: %+v %v", result, err)
	}
	got, err := base64.StdEncoding.DecodeString(broker.gotArgs["chunk"].(string))
	if err != nil || !bytes.Equal(got, big[2<<20:]) {
		t.Fatal("window bytes were not delivered exactly")
	}

	// The whole file in one window is also verified host-side.
	input = mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"site.zip", hex.EncodeToString(bigSum[:]), 0, len(big)}})
	if result, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "whole", Input: input}); err != nil || result.IsError || broker.calls != 2 {
		t.Fatalf("whole-file window refused: %+v %v", result, err)
	}

	// 64 MiB + 1: refused by size, no MCP call.
	over := make([]byte, maxMCPWorkspaceFileBytes+1)
	overSum := sha256.Sum256(over)
	tool, broker = newTool(over, &reads)
	input = mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"site.zip", hex.EncodeToString(overSum[:]), 0, 1 << 20}})
	result, err = tool.Run(context.Background(), fantasy.ToolCall{ID: "over", Input: input})
	if err != nil || !result.IsError || broker.calls != 0 {
		t.Fatalf("64 MiB + 1 file dispatched: %+v %v", result, err)
	}
	// A window over the per-call maximum is refused before the reader runs.
	before := reads
	input = mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"site.zip", hex.EncodeToString(overSum[:]), 0, maxMCPWorkspaceWindowBytes + 1}})
	if result, _ := tool.Run(context.Background(), fantasy.ToolCall{ID: "wide", Input: input}); !result.IsError || reads != before || broker.calls != 0 ||
		!strings.Contains(result.Content, "per-call maximum") {
		t.Fatalf("over-wide window reached the reader: %+v", result)
	}
	// A range reaching past the cap is refused before the reader runs.
	input = mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"site.zip", hex.EncodeToString(overSum[:]), maxMCPWorkspaceFileBytes, 1}})
	if result, _ := tool.Run(context.Background(), fantasy.ToolCall{ID: "past", Input: input}); !result.IsError || reads != before || broker.calls != 0 {
		t.Fatal("range past the cap reached the reader")
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

// TestResolveWorkspaceFileReferencesApprovedArguments covers the approved-call
// form (httpapi runStagedTool): staged arguments decode numbers as
// json.Number, so an integer offset must be accepted there as it is from a
// model's call, while a fraction is still refused before any read.
func TestResolveWorkspaceFileReferencesApprovedArguments(t *testing.T) {
	data := []byte("domain\nexample.com\n")
	sum := sha256.Sum256(data)
	schema := map[string]any{"properties": map[string]any{
		"content_base64": map[string]any{"type": "string", "contentEncoding": "base64"},
		"name":           map[string]any{"type": "string"},
	}}
	decode := func(raw string) map[string]any {
		t.Helper()
		var args map[string]any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			t.Fatal(err)
		}
		return args
	}
	ref := func(offset string) string {
		return fmt.Sprintf(`{"name":"pilot","content_base64":{"workspace_file":"pilot.csv","sha256":%q,"offset":%s,"length":11}}`, hex.EncodeToString(sum[:]), offset)
	}

	if HasWorkspaceFileReferences(schema, decode(`{"name":"pilot","content_base64":"ZG9tYWluCg=="}`)) {
		t.Fatal("inline base64 reported as a reference")
	}
	reads := 0
	args := decode(ref("7"))
	if !HasWorkspaceFileReferences(schema, args) {
		t.Fatal("reference not detected")
	}
	done, err := ResolveWorkspaceFileReferences(context.Background(), schema, fakeWorkspaceReader(t, "pilot.csv", data, &reads), args)
	if err != nil {
		t.Fatalf("json.Number offset refused: %v", err)
	}
	if got, want := args["content_base64"], base64.StdEncoding.EncodeToString(data[7:18]); got != want {
		t.Fatalf("content_base64 = %#v, want %q", got, want)
	}
	done()
	if _, ok := args["content_base64"].(map[string]any); !ok {
		t.Fatal("done did not put the reference back")
	}

	// An integral number is accepted however it is spelled, as it is from a
	// model's call; a fraction is refused before any read.
	for _, offset := range []string{"7.0", "0.7e1"} {
		args := decode(ref(offset))
		done, err := ResolveWorkspaceFileReferences(context.Background(), schema, fakeWorkspaceReader(t, "pilot.csv", data, &reads), args)
		if err != nil {
			t.Fatalf("integral offset %s refused: %v", offset, err)
		}
		if got, want := args["content_base64"], base64.StdEncoding.EncodeToString(data[7:18]); got != want {
			t.Fatalf("offset %s: content_base64 = %#v, want %q", offset, got, want)
		}
		done()
	}
	before := reads
	if _, err := ResolveWorkspaceFileReferences(context.Background(), schema, fakeWorkspaceReader(t, "pilot.csv", data, &reads), decode(ref("0.5"))); err == nil {
		t.Fatal("fractional json.Number offset accepted")
	}
	if reads != before {
		t.Fatal("an invalid reference was read")
	}
}
