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
// (ADR-0085, raising ADR-0066's 2 MiB). 64 MiB matches the largest staged
// upload a Pages-class MCP server accepts, so Fleet is not the bottleneck for a
// chunked upload. Memory stays bounded by the requested window, not this cap:
// the sandbox helper streams the whole file through SHA-256 and returns only
// [offset, offset+length), and the server's maxLength bounds that window
// before anything is read. A single unbounded-maxLength call can still ask for
// the whole 64 MiB in one window; that one read is the worst case.
const maxMCPWorkspaceFileBytes = 64 * 1024 * 1024

// workspaceFileReferenceDescription is the advertised schema text, derived
// from the cap so the number the model reads cannot drift from the one
// enforced below.
var workspaceFileReferenceDescription = fmt.Sprintf("Send the original base64 string OR a workspace file reference. "+
	"Prefer the reference for files: Fleet reads and base64-encodes the exact byte range without exposing bytes to the model. "+
	"sha256 must match the WHOLE file; offset and length are raw bytes. Maximum whole file: %d bytes. "+
	"The tool's existing side effects, approval requirements and verification workflow still apply.", maxMCPWorkspaceFileBytes)

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
		// The reader returns only the requested window plus the SHA-256 of the
		// WHOLE file, computed by the sandbox helper in the same pass that read
		// the window (it streams the file, and refuses one that changed while
		// it was read). Asking for the window instead of the whole file is what
		// keeps a chunked upload linear: n chunks of a large file no longer
		// move n whole copies of it out of the sandbox and through Fleet.
		window, err := m.readWorkspaceFile(ctx, file.Path, int64(file.Offset), int64(file.Length), maxMCPWorkspaceFileBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if window.SHA256 != file.SHA256 {
			return fmt.Errorf("%s: workspace file SHA-256 mismatch; no MCP call was made", name)
		}
		if window.Size > maxMCPWorkspaceFileBytes || int64(len(window.Data)) != int64(file.Length) || int64(file.Offset)+int64(file.Length) > window.Size {
			return fmt.Errorf("%s: byte range exceeds workspace file", name)
		}
		// When the window is the whole file the host re-derives the hash for
		// free, so a whole-file transfer does not rest on the helper's report.
		if file.Offset == 0 && int64(file.Length) == window.Size {
			if sum := sha256.Sum256(window.Data); hex.EncodeToString(sum[:]) != file.SHA256 {
				return fmt.Errorf("%s: workspace file SHA-256 mismatch; no MCP call was made", name)
			}
		}
		args[name] = base64.StdEncoding.EncodeToString(window.Data)
	}
	return nil
}
