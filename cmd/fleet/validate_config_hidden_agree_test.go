package main

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// validate-config's "matches no tool" warning uses the run gate's own naming
// and suffix rule (agentcore.MCPToolName + InteractiveHiddenIn), so it agrees
// with what a chat turn actually hides member by member: a member is reported
// unmatched exactly when the gate would hide none of the bundle's tools.
func TestInteractiveHiddenUnmatched_AgreesWithTheRunGate(t *testing.T) {
	bundle := &clientconfig.Bundle{
		MCPCatalog: []clientconfig.ServerDef{
			{Name: "openx", Tools: []string{"ox_create_deal", "ox_get_deal"}},
			{Name: "ix", Tools: []string{"execute_plan"}},
		},
	}
	tools := []string{
		agentcore.MCPToolName("openx", "ox_create_deal"),
		agentcore.MCPToolName("openx", "ox_get_deal"),
		agentcore.MCPToolName("ix", "execute_plan"),
	}
	for _, member := range []string{
		"ox_create_deal",            // a tool suffix
		"create_deal",               // a shorter suffix on a "_" boundary
		"reate_deal",                // not on a "_" boundary: matches nothing
		"mcp_openx_ox_get_deal",     // the full model-facing name
		"mcp_ix_execute_plan",       // the full name of another server's tool
		"plan",                      // a one-word suffix
		"execute_plan_v2",           // longer than any tool
		"mcp_openx_ox_create_deal_", // a trailing underscore
	} {
		gateHides := false
		for _, n := range tools {
			if agentcore.InteractiveHiddenIn([]string{member}, n) {
				gateHides = true
			}
		}
		unmatched := len(interactiveHiddenUnmatched(bundle, []string{member})) > 0
		if unmatched == gateHides {
			t.Errorf("member %q: validate-config unmatched=%t but the run gate hides a tool=%t", member, unmatched, gateHides)
		}
	}
}
