package mcp

import (
	"slices"
	"testing"
)

// TestGetAllToolsOrderIsStable pins the order the prompt-cache contract
// relies on (docs/PROMPT-CACHE-CONTRACT.md): servers in sorted-name order,
// each server's tools in the order it listed them, identical however the
// servers were registered and however many times the catalog is read.
func TestGetAllToolsOrderIsStable(t *testing.T) {
	servers := map[string][]string{
		"zeta":  {"z2", "z1"},
		"alpha": {"a3", "a1", "a2"},
		"mid":   {"m1"},
		"beta":  {"b1", "b2"},
	}
	want := []string{"alpha/a3", "alpha/a1", "alpha/a2", "beta/b1", "beta/b2", "mid/m1", "zeta/z2", "zeta/z1"}

	for _, order := range [][]string{
		{"zeta", "alpha", "mid", "beta"},
		{"beta", "mid", "alpha", "zeta"},
		{"alpha", "beta", "mid", "zeta"},
	} {
		c := NewClient()
		for _, name := range order {
			s := &Server{name: name}
			for _, tool := range servers[name] {
				s.tools = append(s.tools, Tool{Name: tool})
			}
			c.servers[name] = s
		}
		for range 50 {
			tools := c.GetAllTools()
			got := make([]string, 0, len(tools))
			for _, st := range tools {
				got = append(got, st.ServerName+"/"+st.Tool.Name)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("registered %v: GetAllTools = %v, want %v", order, got, want)
			}
		}
	}
}
