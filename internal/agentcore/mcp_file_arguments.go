package agentcore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const maxMCPWorkspaceFileBytes = 2 * 1024 * 1024

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
			"description": "Send the original base64 string OR a workspace file reference. Prefer the reference for files: Fleet reads and base64-encodes the exact byte range without exposing bytes to the model. sha256 must match the WHOLE file; offset and length are raw bytes. Maximum whole file: 2097152 bytes. The tool's existing side effects, approval requirements and verification workflow still apply.",
			"anyOf": []any{props[name], map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{"workspace_file", "sha256", "offset", "length"},
				"properties": map[string]any{
					"workspace_file": map[string]any{"type": "string", "minLength": 1},
					"sha256":         map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"},
					"offset":         map[string]any{"type": "integer", "minimum": 0, "maximum": maxMCPWorkspaceFileBytes},
					"length":         map[string]any{"type": "integer", "minimum": 1, "maximum": maxMCPWorkspaceFileBytes},
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

func (m *mcpTool) resolveFileReferences(ctx context.Context, args map[string]any) error {
	for name, property := range m.binaryFileProperties() {
		ref, ok := args[name].(map[string]any)
		if !ok {
			continue
		}
		if m.readWorkspaceFile == nil {
			return fmt.Errorf("workspace file references require a sandbox reader")
		}
		if len(ref) != 4 {
			return fmt.Errorf("%s: reference requires exactly workspace_file, sha256, offset and length", name)
		}
		for _, field := range []string{"workspace_file", "sha256", "offset", "length"} {
			if _, exists := ref[field]; !exists {
				return fmt.Errorf("%s: missing reference field %s", name, field)
			}
		}
		if _, ok := ref["offset"].(float64); !ok {
			return fmt.Errorf("%s: offset must be an integer", name)
		}
		raw, err := json.Marshal(ref)
		if err != nil {
			return fmt.Errorf("%s: invalid file reference", name)
		}
		var file workspaceFileArgument
		if json.Unmarshal(raw, &file) != nil || file.Offset < 0 || file.Length <= 0 || file.Offset > maxMCPWorkspaceFileBytes || file.Length > maxMCPWorkspaceFileBytes-file.Offset {
			return fmt.Errorf("%s: invalid file byte range", name)
		}
		hash, err := hex.DecodeString(file.SHA256)
		if err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != file.SHA256 {
			return fmt.Errorf("%s: sha256 must be the lowercase whole-file SHA-256", name)
		}
		// Honor the server's encoded-string bound before reading or dispatching.
		if capValue, exists := property["maxLength"]; exists {
			capJSON, _ := json.Marshal(capValue)
			var maxLength int
			if json.Unmarshal(capJSON, &maxLength) != nil || base64.StdEncoding.EncodedLen(file.Length) > maxLength {
				return fmt.Errorf("%s: byte range exceeds the tool's maxLength", name)
			}
		}
		data, err := m.readWorkspaceFile(ctx, file.Path, maxMCPWorkspaceFileBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return fmt.Errorf("%s: workspace file SHA-256 mismatch; no MCP call was made", name)
		}
		if len(data) > maxMCPWorkspaceFileBytes || file.Offset+file.Length > len(data) {
			return fmt.Errorf("%s: byte range exceeds workspace file", name)
		}
		args[name] = base64.StdEncoding.EncodeToString(data[file.Offset : file.Offset+file.Length])
	}
	return nil
}
