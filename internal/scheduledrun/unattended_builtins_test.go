package scheduledrun

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func toolNames(ts []fantasy.AgentTool) map[string]fantasy.AgentTool {
	out := make(map[string]fantasy.AgentTool, len(ts))
	for _, t := range ts {
		out[t.Info().Name] = t
	}
	return out
}

func unattendedBase() []fantasy.AgentTool {
	return []fantasy.AgentTool{
		tools.NewWebFetchTool(),
		tools.NewCreateTaskTool(tools.CreateTaskConfig{Enqueuer: stubEnqueuer{}}),
		fantasy.NewAgentTool("noop", "noop", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		}),
	}
}

// web_fetch runs host-side, so in an unattended run it follows the sandbox's
// egress posture instead of bypassing it (ADR-0094): absent when the sandbox
// is sealed, held to the allowlist in allowlisted mode, unchanged when egress
// is open. Before this, a sealed scheduled run that read an outside email
// still had web_fetch, an open channel to any URL.
func TestApplyUnattendedBuiltinPolicy_WebFetchFollowsEgress(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{})

	cases := []struct {
		name         string
		allowNetwork bool
		mode         string
		wantFetch    bool
		wantLimited  bool
	}{
		{name: "sealed by default", allowNetwork: false, mode: "", wantFetch: false},
		{name: "sealed even in open mode", allowNetwork: false, mode: sandbox.NetworkModeOpen, wantFetch: false},
		{name: "networked, open mode", allowNetwork: true, mode: sandbox.NetworkModeOpen, wantFetch: true},
		{name: "networked, default mode", allowNetwork: true, mode: "", wantFetch: true},
		{name: "networked, lockdown", allowNetwork: true, mode: sandbox.NetworkModeLockdown, wantFetch: false},
		{name: "networked, allowlisted", allowNetwork: true, mode: sandbox.NetworkModeAllowlisted, wantFetch: true, wantLimited: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &models.Task{ID: uuid.New(), AllowNetwork: tc.allowNetwork}
			got := toolNames(applyUnattendedBuiltinPolicy(unattendedBase(), task, tc.mode, []string{"reports.example.com"}))
			if _, ok := got["noop"]; !ok {
				t.Fatal("unrelated tools must be kept")
			}
			if _, ok := got[tools.CreateTaskToolName]; !ok {
				t.Fatal("create_task is governed by the task's own grant here, not by egress")
			}
			fetch, ok := got[tools.WebFetchToolName]
			if ok != tc.wantFetch {
				t.Fatalf("web_fetch present = %v, want %v", ok, tc.wantFetch)
			}
			if !tc.wantLimited || !ok {
				return
			}
			resp, err := fetch.Run(context.Background(), fantasy.ToolCall{Input: `{"url":"https://elsewhere.example.net/"}`})
			if err != nil {
				t.Fatal(err)
			}
			if !resp.IsError || !strings.Contains(resp.Content, "allowlist") {
				t.Fatalf("allowlisted web_fetch to an unlisted host = %+v, want an allowlist refusal", resp)
			}
		})
	}
}

// agent_policy.unattended_disabled_tools removes the named built-ins from
// every scheduled run, whatever the task's own grants (allow_task_creation,
// allow_network) say.
func TestApplyUnattendedBuiltinPolicy_BundleCanDropBuiltins(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		UnattendedDisabledTools: []string{"create_task", "web_fetch"},
	})
	task := &models.Task{ID: uuid.New(), AllowNetwork: true, AllowTaskCreation: true}
	got := toolNames(applyUnattendedBuiltinPolicy(unattendedBase(), task, sandbox.NetworkModeOpen, nil))
	if _, ok := got[tools.CreateTaskToolName]; ok {
		t.Error("create_task must be removed when the bundle disables it")
	}
	if _, ok := got[tools.WebFetchToolName]; ok {
		t.Error("web_fetch must be removed when the bundle disables it")
	}
	if _, ok := got["noop"]; !ok {
		t.Error("unrelated tools must be kept")
	}
}
