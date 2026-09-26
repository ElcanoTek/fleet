// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package mcp

import (
	"encoding/json"
	"testing"
)

// TestToolAnnotationsDecode: a tools/list entry carrying MCP tool annotations
// keeps them (with absent hints distinguishable from false), and one without
// leaves Annotations nil, so callers can tell "the server said nothing" from
// "the server said no".
func TestToolAnnotationsDecode(t *testing.T) {
	var resp struct {
		Tools []Tool `json:"tools"`
	}
	raw := `{"tools":[
		{"name":"list_x","description":"d","inputSchema":{"type":"object"},"annotations":{"title":"List X","readOnlyHint":true,"destructiveHint":false}},
		{"name":"do_y","description":"d","inputSchema":{"type":"object"}}
	]}`
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	a := resp.Tools[0].Annotations
	if a == nil || a.Title != "List X" || a.ReadOnlyHint == nil || !*a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.IdempotentHint != nil {
		t.Errorf("annotations = %+v; want title, readOnly=true, destructive=false, idempotent absent", a)
	}
	if resp.Tools[1].Annotations != nil {
		t.Errorf("tool without annotations decoded %+v; want nil", resp.Tools[1].Annotations)
	}
	// Re-encoding a tool without annotations must not invent an empty object.
	out, _ := json.Marshal(resp.Tools[1])
	if string(out) != `{"name":"do_y","description":"d","inputSchema":{"type":"object"}}` {
		t.Errorf("re-encoded tool = %s", out)
	}
}
