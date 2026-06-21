package agent

import (
	"encoding/json"
	"strconv"
)

// Tool-result parsing for the scheduled driver's live/session log formatting
// (ported verbatim from cutlass toolresult.go).

// Top-level keys ParseToolResult special-cases when flattening a tool's
// structured JSON output.
const (
	toolResultKeyStdout  = "stdout"
	toolResultKeyStderr  = "stderr"
	toolResultKeyVars    = "vars"
	toolResultKeyIsError = "isError"
)

// ToolResult represents the parsed output of a tool execution.
type ToolResult struct {
	ID      string
	Name    string
	Output  string
	Stdout  string
	Stderr  string
	Vars    map[string]interface{}
	IsError bool
}

// ParseToolResult parses a raw tool output string into a structured ToolResult.
func ParseToolResult(id, name, rawOutput string) ToolResult {
	res := ToolResult{
		ID:     id,
		Name:   name,
		Output: rawOutput,
		Vars:   make(map[string]interface{}),
		Stdout: rawOutput,
	}

	var structured map[string]interface{}
	if err := json.Unmarshal([]byte(rawOutput), &structured); err == nil {
		if stdout, ok := structured[toolResultKeyStdout].(string); ok {
			res.Stdout = stdout
		}
		if stderr, ok := structured[toolResultKeyStderr].(string); ok {
			res.Stderr = stderr
		}
		if vars, ok := structured[toolResultKeyVars].(map[string]interface{}); ok {
			for k, v := range vars {
				res.Vars[k] = v
			}
		}
		if isError, ok := structured[toolResultKeyIsError].(bool); ok {
			res.IsError = isError
		}
		for k, v := range structured {
			if k == toolResultKeyStdout || k == toolResultKeyStderr || k == toolResultKeyVars || k == toolResultKeyIsError {
				continue
			}
			res.Vars[k] = v
		}
		flattenStructuredVars(res.Vars, structured, "")
	}

	return res
}

func flattenStructuredVars(dst map[string]interface{}, value interface{}, prefix string) {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, nested := range v {
			if key == "stdout" || key == "stderr" || key == "vars" || key == "isError" {
				continue
			}
			next := key
			if prefix != "" {
				next = prefix + "_" + key
			}
			flattenStructuredVars(dst, nested, next)
		}
	case []interface{}:
		for i, nested := range v {
			next := prefix + "_" + strconv.Itoa(i)
			flattenStructuredVars(dst, nested, next)
		}
	default:
		if prefix != "" {
			dst[prefix] = v
		}
	}
}
