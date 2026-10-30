package tools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestEnableMCPConnectorsTool_RequiresInteractiveHandler(t *testing.T) {
	tool := NewEnableMCPConnectorsTool()
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  EnableMCPConnectorsToolName,
		Input: `{"connectors":["google_sheets"],"reason":"Need to read spreadsheet"}`,
	})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected tool error response without handler, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "only available in interactive chat") {
		t.Errorf("unexpected error message: %q", resp.Content)
	}
}

func TestEnableMCPConnectorsTool_RejectsEmptyConnectors(t *testing.T) {
	tool := NewEnableMCPConnectorsTool()
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "call-1",
		Name:  EnableMCPConnectorsToolName,
		Input: `{"connectors":["  ", ""]}`,
	})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "connectors is required") {
		t.Fatalf("expected connectors required error, got IsError=%v Content=%q", resp.IsError, resp.Content)
	}
}

func TestEnableMCPConnectorsTool_NormalizesAndPropagatesStopTurn(t *testing.T) {
	var gotCallID string
	var gotParams EnableMCPConnectorsParams
	ctx := WithEnableMCPConnectorsHandler(context.Background(), func(_ context.Context, callID string, params EnableMCPConnectorsParams) (*EnableMCPConnectorsResult, error) {
		gotCallID = callID
		gotParams = params
		return &EnableMCPConnectorsResult{
			Status:       "enabled",
			Connectors:   params.Connectors,
			AutoApproved: true,
			Message:      "Auto-approved and enabled [google_sheets, slack].",
			StopTurn:     true,
		}, nil
	})

	tool := NewEnableMCPConnectorsTool()
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-42",
		Name:  EnableMCPConnectorsToolName,
		Input: `{"connectors":[" Google_Sheets ", "SLACK", "google_sheets"],"reason":"  Pull Q4 sheet  "}`,
	})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("unexpected tool error: %s", resp.Content)
	}
	if !resp.StopTurn {
		t.Errorf("expected StopTurn=true on auto-approved enablement")
	}
	if gotCallID != "call-42" {
		t.Errorf("callID = %q, want call-42", gotCallID)
	}
	if len(gotParams.Connectors) != 2 || gotParams.Connectors[0] != "google_sheets" || gotParams.Connectors[1] != "slack" {
		t.Errorf("normalized connectors = %v, want [google_sheets slack]", gotParams.Connectors)
	}
	if gotParams.Reason != "Pull Q4 sheet" {
		t.Errorf("reason = %q, want %q", gotParams.Reason, "Pull Q4 sheet")
	}
}
