package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestChatPatrolTool_ActionNormalizationAndHandler(t *testing.T) {
	tool := NewChatPatrolTool()
	if tool.Info().Name != ChatPatrolToolName {
		t.Fatalf("tool name = %q, want %q", tool.Info().Name, ChatPatrolToolName)
	}

	// Without a handler in context, returns a graceful tool error.
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "tc-1",
		Name:  ChatPatrolToolName,
		Input: `{"action":"list"}`,
	})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "interactive chat conversations") {
		t.Fatalf("expected missing-handler error, got %+v", resp)
	}

	// With a handler in context, dispatches normalized action and serializes result.
	var gotParams ChatPatrolParams
	ctx := WithChatPatrolHandler(context.Background(), func(_ context.Context, p ChatPatrolParams) (*ChatPatrolResult, error) {
		gotParams = p
		return &ChatPatrolResult{
			Status:      "ok",
			Action:      p.Action,
			Message:     "Scheduled patrol \"Hourly Deal Check\".",
			AutoApprove: true,
			Schedule: &ChatPatrolScheduleView{
				ID:     "sched-1",
				Title:  p.Title,
				Prompt: p.Prompt,
				Kind:   "patrol",
				Cron:   p.Cron,
				Status: "active",
			},
		}, nil
	})

	inputBytes, _ := json.Marshal(ChatPatrolParams{
		Action: "start", // alias for "create"
		Title:  "Hourly Deal Check",
		Prompt: "Check deal pacing and update the dashboard.",
		Cron:   "0 */2 * * *",
	})
	resp, err = tool.Run(ctx, fantasy.ToolCall{
		ID:    "tc-2",
		Name:  ChatPatrolToolName,
		Input: string(inputBytes),
	})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected tool error: %s", resp.Content)
	}
	if gotParams.Action != ChatPatrolActionCreate {
		t.Errorf("normalized action = %q, want %q", gotParams.Action, ChatPatrolActionCreate)
	}
	if !strings.Contains(resp.Content, `"sched-1"`) || !strings.Contains(resp.Content, `"Hourly Deal Check"`) {
		t.Errorf("unexpected response content: %s", resp.Content)
	}
}
