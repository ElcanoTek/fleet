package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// recordingBroker is a test double for the MCPBroker seam: it captures the
// (server, tool, args) of each CallMCP and returns canned results. It lets us
// prove mcpTool delegates the actual call to the broker — the #167 decoupling —
// rather than reaching a *mcp.Client directly.
type recordingBroker struct {
	calls     int
	gotServer string
	gotTool   string
	gotArgs   map[string]any
	text      string
	isErr     bool
	err       error
}

func (b *recordingBroker) CallMCP(_ context.Context, server, tool string, args map[string]any) (string, bool, error) {
	b.calls++
	b.gotServer, b.gotTool, b.gotArgs = server, tool, args
	return b.text, b.isErr, b.err
}

// gatePolicy captures BeforeToolCall / RecordToolResult so we can assert the
// per-call framing mcpTool keeps around the broker call (gate + result record).
type gatePolicy struct {
	block      bool
	blockMsg   string
	recorded   bool
	recordName string
	recordText string
	recordOK   bool
}

func (p *gatePolicy) BeforeToolCall(_, _, _ string) (bool, string) { return p.block, p.blockMsg }
func (p *gatePolicy) RecordToolResult(name, _, resultText string, succeeded bool) {
	p.recorded, p.recordName, p.recordText, p.recordOK = true, name, resultText, succeeded
}
func (p *gatePolicy) CanFinish(int) (bool, []string) { return true, nil }

func newTestMCPTool(broker MCPBroker, policy Policy) *mcpTool {
	return &mcpTool{
		serverName: "deal_sheet",
		tool:       mcp.Tool{Name: "lookup", Description: "d"},
		broker:     broker,
		policy:     policy,
	}
}

// TestMCPTool_RoutesCallThroughBroker is the core #167 assertion: mcpTool forwards
// the call to its injected MCPBroker (with the parsed args, server, and tool), and
// renders a successful text result back. The decoupling means the broker — not a
// hard-wired *mcp.Client — owns where the call physically happens.
func TestMCPTool_RoutesCallThroughBroker(t *testing.T) {
	broker := &recordingBroker{text: "hello-result"}
	tool := newTestMCPTool(broker, nil)

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Input: `{"q":"x","n":2}`})
	if err != nil {
		t.Fatalf("Run returned a Go error, want none: %v", err)
	}
	if broker.calls != 1 {
		t.Fatalf("broker.CallMCP called %d times, want 1", broker.calls)
	}
	if broker.gotServer != "deal_sheet" || broker.gotTool != "lookup" {
		t.Fatalf("broker got (%q, %q), want (deal_sheet, lookup)", broker.gotServer, broker.gotTool)
	}
	if broker.gotArgs["q"] != "x" {
		t.Fatalf("broker got args %v, want q=x decoded from the tool input", broker.gotArgs)
	}
	if resp.IsError || resp.Content != "hello-result" {
		t.Fatalf("resp = (content=%q, isError=%v), want (hello-result, false)", resp.Content, resp.IsError)
	}
}

// TestMCPTool_IsErrorMapsToErrorResponse: a tool-level isError from the broker
// (MCP 2025-06-18: a successful response with isError=true) becomes a fantasy
// error response carrying the broker's text — not a Go error.
func TestMCPTool_IsErrorMapsToErrorResponse(t *testing.T) {
	broker := &recordingBroker{text: "boom", isErr: true}
	resp, err := newTestMCPTool(broker, nil).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: "{}"})
	if err != nil {
		t.Fatalf("Run returned a Go error, want none: %v", err)
	}
	if !resp.IsError || resp.Content != "boom" {
		t.Fatalf("resp = (content=%q, isError=%v), want (boom, true)", resp.Content, resp.IsError)
	}
}

// TestMCPTool_EmptyIsErrorGetsSyntheticText: isError with no text still surfaces a
// non-empty error message so the model and log know the call failed.
func TestMCPTool_EmptyIsErrorGetsSyntheticText(t *testing.T) {
	broker := &recordingBroker{text: "", isErr: true}
	resp, _ := newTestMCPTool(broker, nil).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: "{}"})
	if !resp.IsError || !strings.Contains(resp.Content, "isError=true with no text") {
		t.Fatalf("resp = (content=%q, isError=%v), want a synthetic isError message", resp.Content, resp.IsError)
	}
}

// TestMCPTool_TransportErrorMapsToErrorResponse: a transport error from the broker
// (distinct from a tool-level isError) is mapped to an error response naming the
// tool, and is NOT propagated as a Go error (the loop must not abort the turn).
func TestMCPTool_TransportErrorMapsToErrorResponse(t *testing.T) {
	broker := &recordingBroker{err: errors.New("dial fail")}
	resp, err := newTestMCPTool(broker, nil).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: "{}"})
	if err != nil {
		t.Fatalf("Run returned a Go error, want it mapped to a response: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "Error calling mcp_deal_sheet_lookup") || !strings.Contains(resp.Content, "dial fail") {
		t.Fatalf("resp content = %q, want an 'Error calling ...: dial fail' error response", resp.Content)
	}
}

// TestMCPTool_PolicyGateBlocksBeforeBroker: when the policy blocks the call,
// mcpTool returns the block message WITHOUT ever reaching the broker.
func TestMCPTool_PolicyGateBlocksBeforeBroker(t *testing.T) {
	broker := &recordingBroker{text: "should-not-run"}
	pol := &gatePolicy{block: true, blockMsg: "denied by policy"}
	resp, err := newTestMCPTool(broker, pol).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: "{}"})
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if broker.calls != 0 {
		t.Fatalf("broker was called %d times, want 0 (policy blocked before the call)", broker.calls)
	}
	if !resp.IsError || resp.Content != "denied by policy" {
		t.Fatalf("resp = (content=%q, isError=%v), want the block message", resp.Content, resp.IsError)
	}
}

// TestMCPTool_RecordsResultThroughPolicy: after a call, mcpTool records the outcome
// via the policy seam — success on a clean result, failure on a tool-level isError.
func TestMCPTool_RecordsResultThroughPolicy(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		pol := &gatePolicy{}
		broker := &recordingBroker{text: "ok-text"}
		if _, err := newTestMCPTool(broker, pol).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: "{}"}); err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if !pol.recorded || pol.recordName != "mcp_deal_sheet_lookup" || pol.recordText != "ok-text" || !pol.recordOK {
			t.Fatalf("record = {recorded:%v name:%q text:%q ok:%v}, want a successful record of ok-text",
				pol.recorded, pol.recordName, pol.recordText, pol.recordOK)
		}
	})
	t.Run("isError records failure", func(t *testing.T) {
		pol := &gatePolicy{}
		broker := &recordingBroker{text: "nope", isErr: true}
		if _, err := newTestMCPTool(broker, pol).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: "{}"}); err != nil {
			t.Fatalf("Run error: %v", err)
		}
		if !pol.recorded || pol.recordOK {
			t.Fatalf("record = {recorded:%v ok:%v}, want a recorded FAILURE", pol.recorded, pol.recordOK)
		}
	})
}

// TestMCPTool_InvalidArgsShortCircuits: malformed tool input is rejected before the
// broker is reached (no call, an error response the model can correct from).
func TestMCPTool_InvalidArgsShortCircuits(t *testing.T) {
	broker := &recordingBroker{}
	resp, _ := newTestMCPTool(broker, nil).Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: "not-json"})
	if broker.calls != 0 {
		t.Fatalf("broker called %d times on invalid args, want 0", broker.calls)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "invalid arguments") {
		t.Fatalf("resp content = %q, want an 'invalid arguments' error response", resp.Content)
	}
}

// ── localMCPBroker (the in-process implementation) ──
//
// These pin the call → flatten → isError rendering the broker took over from
// mcpTool.Run, plus the fast.io guard, against a faked client surface (mcpCaller).

// fakeCaller is a test double for the minimal client surface localMCPBroker needs.
type fakeCaller struct {
	calls  int
	result *mcp.ToolResult
	err    error
}

func (c *fakeCaller) CallToolOn(_ context.Context, _, _ string, _ map[string]any) (*mcp.ToolResult, error) {
	c.calls++
	return c.result, c.err
}

// TestLocalMCPBroker_FlattensTextBlocks: the broker joins the text content blocks
// (each followed by a newline) and skips non-text blocks — the rendering mcpTool
// previously did inline.
func TestLocalMCPBroker_FlattensTextBlocks(t *testing.T) {
	caller := &fakeCaller{result: &mcp.ToolResult{Content: []mcp.ContentBlock{
		{Type: "text", Text: "line1"},
		{Type: "image", Text: "ignored-non-text"},
		{Type: "text", Text: "line2"},
	}}}
	b := &localMCPBroker{client: caller, hints: DefaultRemediationHints}

	text, isErr, err := b.CallMCP(context.Background(), "deal_sheet", "lookup", map[string]any{})
	if err != nil {
		t.Fatalf("CallMCP err: %v", err)
	}
	if isErr {
		t.Fatalf("isErr = true, want false")
	}
	if text != "line1\nline2\n" {
		t.Fatalf("text = %q, want only the text blocks flattened (newline-joined)", text)
	}
}

// TestLocalMCPBroker_IsErrorPassthrough: a tool-level isError result is surfaced as
// isError=true (not a Go error), carrying the flattened text.
func TestLocalMCPBroker_IsErrorPassthrough(t *testing.T) {
	caller := &fakeCaller{result: &mcp.ToolResult{
		Content: []mcp.ContentBlock{{Type: "text", Text: "bad"}},
		IsError: true,
	}}
	b := &localMCPBroker{client: caller, hints: DefaultRemediationHints}

	text, isErr, err := b.CallMCP(context.Background(), "s", "t", nil)
	if err != nil {
		t.Fatalf("CallMCP err: %v", err)
	}
	if !isErr || text != "bad\n" {
		t.Fatalf("(text=%q, isErr=%v), want (bad\\n, true)", text, isErr)
	}
}

// TestLocalMCPBroker_TransportErrorPropagates: a transport error from the client is
// returned as a Go error (distinct from a tool-level isError) — the caller (mcpTool)
// maps it to a model-facing error response.
func TestLocalMCPBroker_TransportErrorPropagates(t *testing.T) {
	caller := &fakeCaller{err: errors.New("dial fail")}
	b := &localMCPBroker{client: caller, hints: DefaultRemediationHints}

	_, _, err := b.CallMCP(context.Background(), "s", "t", nil)
	if err == nil || !strings.Contains(err.Error(), "dial fail") {
		t.Fatalf("err = %v, want the transport error propagated", err)
	}
}

// TestLocalMCPBroker_FastIOGuardFiresBeforeClient: an oversized fast.io inline-base64
// upload is rejected (isError + hint) BEFORE the client is reached, so a hostile or
// buggy payload never hits the wire.
func TestLocalMCPBroker_FastIOGuardFiresBeforeClient(t *testing.T) {
	caller := &fakeCaller{result: &mcp.ToolResult{}}
	b := &localMCPBroker{client: caller, hints: DefaultRemediationHints}

	big := strings.Repeat("A", 64*1024)
	text, isErr, err := b.CallMCP(context.Background(), "fast_io", "upload", map[string]any{
		"action":         "stream_upload",
		"content_base64": big,
	})
	if err != nil {
		t.Fatalf("CallMCP returned a transport error, want a tool-level reject: %v", err)
	}
	if !isErr {
		t.Fatalf("oversized fast.io inline upload should be rejected (isErr=true)")
	}
	if caller.calls != 0 {
		t.Fatalf("client.CallToolOn called %d times, the guard must fire before the wire", caller.calls)
	}
	if !strings.Contains(text, "Rejected locally") {
		t.Fatalf("reject hint = %q, want the inline-upload guard message", text)
	}
}
