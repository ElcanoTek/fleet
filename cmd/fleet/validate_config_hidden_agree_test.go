package main

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// validate-config's "matches no tool" warning follows the run gate's naming
// (mcp_<server>_<tool>) and its critical_tools suffix rule (a whole name, or
// a suffix on a "_" boundary). Each case states what the gate hides, so a
// change to either side that makes them disagree fails here.
func TestInteractiveHiddenUnmatched_FollowsTheRunGateRule(t *testing.T) {
	bundle := &clientconfig.Bundle{
		MCPCatalog: []clientconfig.ServerDef{
			{Name: "openx", Tools: []string{"ox_create_deal", "ox_get_deal"}},
			{Name: "ix", Tools: []string{"execute_plan"}},
		},
		HTTPTools: []clientconfig.HTTPToolDef{{Name: "lookup_rate"}},
	}
	for _, tc := range []struct {
		member  string
		matches bool
	}{
		{"ox_create_deal", true},             // a tool name
		{"create_deal", true},                // a shorter suffix on a "_" boundary
		{"reate_deal", false},                // not on a "_" boundary
		{"mcp_openx_ox_get_deal", true},      // the whole model-facing name
		{"openx_ox_get_deal", true},          // server + tool, a suffix of the whole name
		{"plan", true},                       // a one-word suffix
		{"execute_plan_v2", false},           // longer than any tool
		{"mcp_openx_ox_create_deal_", false}, // a trailing underscore
		{"mcp_ix_ox_create_deal", false},     // the right tool on the wrong server
		{"lookup_rate", true},                // an http_tools entry
	} {
		unmatched := len(interactiveHiddenUnmatched(bundle, []string{tc.member})) > 0
		if unmatched == tc.matches {
			t.Errorf("member %q: unmatched=%t, want matches=%t", tc.member, unmatched, tc.matches)
		}
	}
}

// A member that pins a connector by name hides only the base seat: a
// named-account seat registers as mcp_<server>_<account>_<tool>. It warns
// and names the tool to list instead. A tool whose own name starts with its
// server's name (ix's ix_merge_deal_domains) is not mistaken for one.
func TestInteractiveHiddenServerPinned(t *testing.T) {
	bundle := &clientconfig.Bundle{
		MCPCatalog: []clientconfig.ServerDef{
			{Name: "openx", Tools: []string{"ox_create_deal"}},
			{Name: "ix", Tools: []string{"ix_merge_deal_domains"}},
		},
	}
	got := interactiveHiddenServerPinned(bundle, []string{
		"openx_ox_create_deal", "mcp_openx_ox_create_deal", "ox_create_deal",
		"ix_merge_deal_domains", "ix_ix_merge_deal_domains",
	})
	if len(got) != 3 {
		t.Fatalf("want 3 warnings (openx_…, mcp_openx_…, ix_ix_…), got %q", got)
	}
	for i, member := range []string{"openx_ox_create_deal", "mcp_openx_ox_create_deal", "ix_ix_merge_deal_domains"} {
		if !strings.Contains(got[i], `"`+member+`"`) || !strings.Contains(got[i], "<account>") {
			t.Fatalf("warning %d = %q", i, got[i])
		}
	}
	if !strings.Contains(got[0], `list "ox_create_deal" instead`) || !strings.Contains(got[2], `list "ix_merge_deal_domains" instead`) {
		t.Fatalf("warnings must name the tool to list: %q", got)
	}
}
