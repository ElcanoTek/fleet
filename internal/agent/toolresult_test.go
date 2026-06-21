package agent

import (
	"reflect"
	"testing"
)

func TestParseToolResult(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		toolName  string
		rawOutput string
		want      ToolResult
	}{
		{
			name:      "plain text output",
			id:        "123",
			toolName:  "echo",
			rawOutput: "hello world\n",
			want: ToolResult{
				ID:      "123",
				Name:    "echo",
				Output:  "hello world\n",
				Stdout:  "hello world\n",
				Stderr:  "",
				Vars:    map[string]interface{}{},
				IsError: false,
			},
		},
		{
			name:      "JSON with stdout, stderr, and vars",
			id:        "124",
			toolName:  "jsonTool",
			rawOutput: `{"stdout": "json hello", "stderr": "json error", "vars": {"var1": "value1", "var2": 2}}`,
			want: ToolResult{
				ID:     "124",
				Name:   "jsonTool",
				Output: `{"stdout": "json hello", "stderr": "json error", "vars": {"var1": "value1", "var2": 2}}`,
				Stdout: "json hello",
				Stderr: "json error",
				Vars: map[string]interface{}{
					"var1": "value1",
					"var2": float64(2), // JSON numbers unmarshal to float64
				},
				IsError: false,
			},
		},
		{
			name:      "JSON with additional top-level keys",
			id:        "125",
			toolName:  "extraKeysTool",
			rawOutput: `{"stdout": "out", "extra_key": "extra_value", "number_key": 42}`,
			want: ToolResult{
				ID:     "125",
				Name:   "extraKeysTool",
				Output: `{"stdout": "out", "extra_key": "extra_value", "number_key": 42}`,
				Stdout: "out",
				Stderr: "",
				Vars: map[string]interface{}{
					"extra_key":  "extra_value",
					"number_key": float64(42),
				},
				IsError: false,
			},
		},
		{
			name:      "JSON with nested object flattening",
			id:        "126",
			toolName:  "nestedMapTool",
			rawOutput: `{"parent": {"child": "value", "sub": {"deep": true}}}`,
			want: ToolResult{
				ID:     "126",
				Name:   "nestedMapTool",
				Output: `{"parent": {"child": "value", "sub": {"deep": true}}}`,
				Stdout: `{"parent": {"child": "value", "sub": {"deep": true}}}`,
				Stderr: "",
				Vars: map[string]interface{}{
					"parent": map[string]interface{}{
						"child": "value",
						"sub": map[string]interface{}{
							"deep": true,
						},
					},
					"parent_child":    "value",
					"parent_sub_deep": true,
				},
				IsError: false,
			},
		},
		{
			name:      "JSON with nested array flattening",
			id:        "127",
			toolName:  "nestedArrayTool",
			rawOutput: `{"list": ["a", "b", {"c": "d"}]}`,
			want: ToolResult{
				ID:     "127",
				Name:   "nestedArrayTool",
				Output: `{"list": ["a", "b", {"c": "d"}]}`,
				Stdout: `{"list": ["a", "b", {"c": "d"}]}`,
				Stderr: "",
				Vars: map[string]interface{}{
					"list": []interface{}{
						"a",
						"b",
						map[string]interface{}{"c": "d"},
					},
					"list_0":   "a",
					"list_1":   "b",
					"list_2_c": "d",
				},
				IsError: false,
			},
		},
		{
			name:      "JSON mixed flattening with ignored top-level keys in recursion",
			id:        "128",
			toolName:  "mixedTool",
			rawOutput: `{"config": {"stdout": "ignored", "vars": "ignored", "other": "valid"}}`,
			want: ToolResult{
				ID:     "128",
				Name:   "mixedTool",
				Output: `{"config": {"stdout": "ignored", "vars": "ignored", "other": "valid"}}`,
				Stdout: `{"config": {"stdout": "ignored", "vars": "ignored", "other": "valid"}}`,
				Stderr: "",
				Vars: map[string]interface{}{
					"config": map[string]interface{}{
						"stdout": "ignored",
						"vars":   "ignored",
						"other":  "valid",
					},
					"config_other": "valid",
				},
				IsError: false,
			},
		},
		{
			name:      "Malformed JSON falls back to raw output",
			id:        "129",
			toolName:  "malformedTool",
			rawOutput: `{"stdout": "out", "unfinished_json`,
			want: ToolResult{
				ID:      "129",
				Name:    "malformedTool",
				Output:  `{"stdout": "out", "unfinished_json`,
				Stdout:  `{"stdout": "out", "unfinished_json`,
				Stderr:  "",
				Vars:    map[string]interface{}{},
				IsError: false,
			},
		},
		{
			name:      "JSON with isError=true",
			id:        "130",
			toolName:  "errorTool",
			rawOutput: `{"isError": true, "stdout": "failed"}`,
			want: ToolResult{
				ID:      "130",
				Name:    "errorTool",
				Output:  `{"isError": true, "stdout": "failed"}`,
				Stdout:  "failed",
				Stderr:  "",
				Vars:    map[string]interface{}{},
				IsError: true,
			},
		},
		{
			name:      "JSON with isError=false",
			id:        "131",
			toolName:  "successTool",
			rawOutput: `{"isError": false, "stdout": "success"}`,
			want: ToolResult{
				ID:      "131",
				Name:    "successTool",
				Output:  `{"isError": false, "stdout": "success"}`,
				Stdout:  "success",
				Stderr:  "",
				Vars:    map[string]interface{}{},
				IsError: false,
			},
		},
		{
			name:      "JSON with wrong types for reserved keys",
			id:        "132",
			toolName:  "wrongTypesTool",
			rawOutput: `{"isError": "yes", "stdout": 123, "stderr": true, "vars": "notamap"}`,
			want: ToolResult{
				ID:      "132",
				Name:    "wrongTypesTool",
				Output:  `{"isError": "yes", "stdout": 123, "stderr": true, "vars": "notamap"}`,
				Stdout:  `{"isError": "yes", "stdout": 123, "stderr": true, "vars": "notamap"}`,
				Stderr:  "",
				Vars:    map[string]interface{}{},
				IsError: false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseToolResult(tt.id, tt.toolName, tt.rawOutput)

			if got.ID != tt.want.ID {
				t.Errorf("ID = %v, want %v", got.ID, tt.want.ID)
			}
			if got.Name != tt.want.Name {
				t.Errorf("Name = %v, want %v", got.Name, tt.want.Name)
			}
			if got.Output != tt.want.Output {
				t.Errorf("Output = %v, want %v", got.Output, tt.want.Output)
			}
			if got.Stdout != tt.want.Stdout {
				t.Errorf("Stdout = %v, want %v", got.Stdout, tt.want.Stdout)
			}
			if got.Stderr != tt.want.Stderr {
				t.Errorf("Stderr = %v, want %v", got.Stderr, tt.want.Stderr)
			}
			if got.IsError != tt.want.IsError {
				t.Errorf("IsError = %v, want %v", got.IsError, tt.want.IsError)
			}
			if !reflect.DeepEqual(got.Vars, tt.want.Vars) {
				t.Errorf("Vars = %v, want %v", got.Vars, tt.want.Vars)
			}
		})
	}
}
