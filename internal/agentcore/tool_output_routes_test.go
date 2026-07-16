package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
	fleettools "github.com/ElcanoTek/fleet/internal/tools"
)

func TestBuildFantasyTools_AllRegistrationRoutesUseFinalBoundary(t *testing.T) {
	t.Cleanup(func() {
		SetMaxToolOutputBytes(-1)
		SetToolDisclosureThreshold(0)
	})
	SetMaxToolOutputBytes(2048)
	SetToolDisclosureThreshold(100)
	payload := `{"items":["` + strings.Repeat("ordinary row value ", 5000) + `"],"next":"cursor"}`
	tool := func(name string) fantasy.AgentTool {
		return fantasy.NewAgentTool(name, "large structured result",
			func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse(payload), nil
			})
	}
	broker := &hugeBroker{payload: payload}
	catalog := []mcp.ServerTool{{ServerName: "srv", Tool: mcp.Tool{
		Name: "dump", Description: "large MCP result", InputSchema: map[string]any{"type": "object"},
	}}}
	registered, err := buildFantasyTools(
		[]fantasy.AgentTool{tool("native_large")}, catalog, broker, nil, nil, nil, nil,
		toolBuildConfig{loaderTools: []fantasy.AgentTool{tool("loader_large")}},
	)
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	for _, name := range []string{"native_large", "loader_large", "mcp_srv_dump"} {
		response := runRegisteredTool(t, registered, name, "{}")
		assertBoundedStructuredEnvelope(t, name, response.Content, 2048, len(payload))
	}
}

func TestMCPFinalBoundaryParity_DirectDeferredAndConcurrent(t *testing.T) {
	t.Cleanup(func() {
		SetMaxToolOutputBytes(-1)
		SetToolDisclosureThreshold(0)
	})
	SetMaxToolOutputBytes(4096)
	payload := `{"records":["` + strings.Repeat("governed record content ", 6000) + `"]}`
	broker := &hugeBroker{payload: payload}
	catalog := []mcp.ServerTool{{ServerName: "srv", Tool: mcp.Tool{
		Name: "dump", Description: "large MCP result", InputSchema: map[string]any{"type": "object"},
	}}}

	SetToolDisclosureThreshold(100)
	directTools, err := buildFantasyTools(nil, catalog, broker, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("direct build: %v", err)
	}
	direct := runRegisteredTool(t, directTools, "mcp_srv_dump", "{}").Content

	// One core tool pushes directTotal above a threshold of one, forcing the MCP
	// tool behind tool_call while retaining the same hidden boundary wrapper.
	SetToolDisclosureThreshold(1)
	core := fantasy.NewAgentTool("core", "core", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	deferredTools, err := buildFantasyTools([]fantasy.AgentTool{core}, catalog, broker, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("deferred build: %v", err)
	}
	deferredInput := `{"name":"mcp_srv_dump","arguments":{}}`
	deferred := runRegisteredTool(t, deferredTools, "tool_call", deferredInput).Content
	if direct != deferred {
		t.Fatalf("direct/deferred boundary drift:\ndirect=%s\ndeferred=%s", direct, deferred)
	}
	assertBoundedStructuredEnvelope(t, "direct/deferred", direct, 4096, len(payload))

	// Exercise the same wrappers concurrently; this test is intentionally useful
	// under -race because MCP routes and artifact metrics are process-shared.
	var wg sync.WaitGroup
	errCh := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			resp, err := findRegisteredTool(directTools, "mcp_srv_dump").Run(context.Background(), fantasy.ToolCall{ID: fmt.Sprintf("direct-%d", i), Name: "mcp_srv_dump", Input: "{}"})
			if err != nil {
				errCh <- fmt.Errorf("direct %d: %w", i, err)
				return
			}
			if len(resp.Content) > 4096 || !json.Valid([]byte(resp.Content)) {
				errCh <- fmt.Errorf("direct %d: bytes=%d valid=%t", i, len(resp.Content), json.Valid([]byte(resp.Content)))
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			resp, err := findRegisteredTool(deferredTools, "tool_call").Run(context.Background(), fantasy.ToolCall{ID: fmt.Sprintf("deferred-%d", i), Name: "tool_call", Input: deferredInput})
			if err != nil {
				errCh <- fmt.Errorf("deferred %d: %w", i, err)
				return
			}
			if len(resp.Content) > 4096 || !json.Valid([]byte(resp.Content)) {
				errCh <- fmt.Errorf("deferred %d: bytes=%d valid=%t", i, len(resp.Content), json.Valid([]byte(resp.Content)))
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestFinalBoundary_RouteMediaSuppressed(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(1024)
	media := fantasy.NewAgentTool("native_media", "returns raw media",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewMediaResponse([]byte(strings.Repeat("raw-video", 1000)), "video/mp4"), nil
		})
	registered, err := buildFantasyTools([]fantasy.AgentTool{media}, nil, nil, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	response := runRegisteredTool(t, registered, "native_media", "{}")
	if len(response.Data) != 0 || response.Type != "text" || len(response.Content) > 1024 {
		t.Fatalf("media route bypassed boundary: type=%s data=%d text=%d", response.Type, len(response.Data), len(response.Content))
	}
	var envelope toolOutputEnvelope
	if err := json.Unmarshal([]byte(response.Content), &envelope); err != nil || !envelope.BinarySuppressed {
		t.Fatalf("media suppression envelope: err=%v value=%+v", err, envelope)
	}
}

func TestFinalBoundary_ConcurrentConversationArtifacts(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(2048)
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	ctx := fleettools.WithConversationID(context.Background(), "conv-boundary-race")
	payload := `{"rows":["` + strings.Repeat("race safe row ", 8000) + `"]}`
	wrapped := withModelOutputBoundary(fantasy.NewAgentTool("race_tool", "race tool",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(payload), nil
		}))

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("call-%d", i), Name: "race_tool", Input: "{}"})
			if err != nil {
				errCh <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if !json.Valid([]byte(resp.Content)) || len(resp.Content) > 2048 {
				errCh <- fmt.Errorf("call %d: bytes=%d valid=%t", i, len(resp.Content), json.Valid([]byte(resp.Content)))
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func runRegisteredTool(t *testing.T, registered []fantasy.AgentTool, name, input string) fantasy.ToolResponse {
	t.Helper()
	tool := findRegisteredTool(registered, name)
	if tool == nil {
		t.Fatalf("registered tool %q not found", name)
	}
	response, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-" + name, Name: name, Input: input})
	if err != nil {
		t.Fatalf("%s Run: %v", name, err)
	}
	return response
}

func findRegisteredTool(registered []fantasy.AgentTool, name string) fantasy.AgentTool {
	for _, tool := range registered {
		if tool.Info().Name == name {
			return tool
		}
	}
	return nil
}

func assertBoundedStructuredEnvelope(t *testing.T, route, content string, limit, originalBytes int) {
	t.Helper()
	if len(content) > limit {
		t.Fatalf("%s content = %d bytes, want <= %d", route, len(content), limit)
	}
	var envelope toolOutputEnvelope
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		t.Fatalf("%s structured result is invalid JSON: %v\n%s", route, err, content)
	}
	if !envelope.Truncated || envelope.OriginalBytes != originalBytes || envelope.RecoveryAction == "" {
		t.Fatalf("%s envelope = %+v", route, envelope)
	}
}
