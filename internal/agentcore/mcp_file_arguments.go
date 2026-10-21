package agentcore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// maxMCPWorkspaceFileBytes caps the WHOLE workspace file a reference may name
// (ADR-0087, raising ADR-0066's 2 MiB). 64 MiB matches the largest staged
// upload a Pages-class MCP server accepts, so Fleet is not the bottleneck for a
// chunked upload. The file is read out of the sandbox and hashed in Fleet once
// per reference, under a process-wide memory budget for cached and in-flight
// reads; each window is sliced from the verified bytes (tools.WorkspaceFileReader).
const maxMCPWorkspaceFileBytes = 64 * 1024 * 1024

// maxMCPWorkspaceWindowBytes caps the raw bytes ONE call may carry from a
// reference (ADR-0087). Every window is charged, with all its dispatch
// copies, to the process-wide workspace-file budget until the call returns
// (tools.WorkspaceWindowCharge); 8 MiB is also the largest Pages upload
// chunk. A larger file goes as several calls.
const maxMCPWorkspaceWindowBytes = 8 * 1024 * 1024

// workspaceFileReferenceDescription is the advertised schema text, derived
// from the cap so the number the model reads cannot drift from the one
// enforced below.
var workspaceFileReferenceDescription = fmt.Sprintf("Send the original base64 string OR a workspace file reference. "+
	"Prefer the reference for files: Fleet reads and base64-encodes the exact byte range without exposing bytes to the model. "+
	"sha256 must match the WHOLE file; offset and length are raw bytes. Maximum whole file: %d bytes; maximum length per call: %d bytes. "+
	"The tool's existing side effects, approval requirements and verification workflow still apply.", maxMCPWorkspaceFileBytes, maxMCPWorkspaceWindowBytes)

// MCP servers opt in per binary string property with the standard JSON Schema
// contentEncoding annotation. No connector identity or upload protocol lives in
// Fleet. The original tool still crosses all gates and the credential broker;
// only the encoding step moves out of the model's context.
func (m *mcpTool) binaryFileProperties() map[string]map[string]any {
	result := map[string]map[string]any{}
	props, _ := m.tool.InputSchema["properties"].(map[string]any)
	for name, value := range props {
		property, ok := value.(map[string]any)
		if ok && property["type"] == "string" && property["contentEncoding"] == "base64" {
			result[name] = property
		}
	}
	return result
}

func (m *mcpTool) advertiseFileReferences(props map[string]any) {
	if m.readWorkspaceFile == nil {
		return
	}
	for name := range m.binaryFileProperties() {
		props[name] = map[string]any{
			"description": workspaceFileReferenceDescription,
			"anyOf": []any{props[name], map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{"workspace_file", "sha256", "offset", "length"},
				"properties": map[string]any{
					"workspace_file": map[string]any{"type": "string", "minLength": 1},
					"sha256":         map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
					"offset":         map[string]any{"type": "integer", "minimum": 0, "maximum": maxMCPWorkspaceFileBytes - 1},
					"length":         map[string]any{"type": "integer", "minimum": 1, "maximum": maxMCPWorkspaceWindowBytes},
				},
			}},
		}
	}
}

type workspaceFileArgument struct {
	Path   string `json:"workspace_file"`
	SHA256 string `json:"sha256"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// resolveFileReferences replaces each workspace-file reference in args with
// the base64 of its window. On success the caller MUST defer the returned
// done until the outbound call has returned: it puts the references back
// (dropping the base64 strings) and releases each window's charge against the
// workspace-file budget (ADR-0087). On error, everything acquired so far is
// already released.
func (m *mcpTool) resolveFileReferences(ctx context.Context, args map[string]any) (done func(), err error) {
	var undo []func()
	cleanup := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		undo = nil
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	for name, property := range m.binaryFileProperties() {
		ref, ok := args[name].(map[string]any)
		if !ok {
			continue
		}
		if m.readWorkspaceFile == nil {
			return nil, fmt.Errorf("workspace file references require a sandbox reader")
		}
		if len(ref) != 4 {
			return nil, fmt.Errorf("%s: reference requires exactly workspace_file, sha256, offset and length", name)
		}
		for _, field := range []string{"workspace_file", "sha256", "offset", "length"} {
			if _, exists := ref[field]; !exists {
				return nil, fmt.Errorf("%s: missing reference field %s", name, field)
			}
		}
		if _, ok := ref["offset"].(float64); !ok {
			return nil, fmt.Errorf("%s: offset must be an integer", name)
		}
		raw, err := json.Marshal(ref)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid file reference", name)
		}
		var file workspaceFileArgument
		if json.Unmarshal(raw, &file) != nil || file.Offset < 0 || file.Length <= 0 || file.Offset > maxMCPWorkspaceFileBytes || file.Length > maxMCPWorkspaceFileBytes-file.Offset {
			return nil, fmt.Errorf("%s: invalid file byte range", name)
		}
		if file.Length > maxMCPWorkspaceWindowBytes {
			return nil, fmt.Errorf("%s: length exceeds the %d-byte per-call maximum; send the file in several calls", name, maxMCPWorkspaceWindowBytes)
		}
		hash, err := hex.DecodeString(file.SHA256)
		if err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != file.SHA256 {
			return nil, fmt.Errorf("%s: sha256 must be the lowercase whole-file SHA-256", name)
		}
		// Honor the server's encoded-string bound before reading or dispatching.
		if capValue, exists := property["maxLength"]; exists {
			capJSON, _ := json.Marshal(capValue)
			var maxLength int
			if json.Unmarshal(capJSON, &maxLength) != nil || base64.StdEncoding.EncodedLen(file.Length) > maxLength {
				return nil, fmt.Errorf("%s: byte range exceeds the tool's maxLength", name)
			}
		}
		// The reader hashes the WHOLE file in Fleet — never trusting a digest
		// computed inside the model-writable sandbox — and returns the window
		// from those verified bytes, kept in a bounded cache so the next chunk
		// of the same reference is a slice, not another read. The window, the
		// file it is cut from, and every dispatch copy of its base64 stay
		// charged to the process-wide budget until release (ADR-0087).
		window, release, err := m.readWorkspaceFile(ctx, file.Path, file.SHA256, int64(file.Offset), int64(file.Length), maxMCPWorkspaceFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		undo = append(undo, release)
		if len(window) != file.Length {
			return nil, fmt.Errorf("%s: byte range exceeds workspace file", name)
		}
		args[name] = base64.StdEncoding.EncodeToString(window)
		undo = append(undo, func() { args[name] = ref })
	}
	return cleanup, nil
}
