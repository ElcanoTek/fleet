package agentcore

import (
	"context"
	"strings"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// MCPBroker is the ONE seam every MCP tool invocation funnels through.
// It runs a single MCP server.tool call and returns the flattened text content,
// the MCP tool-level isError bit (per the 2025-06-18 spec a tool-level failure is
// a successful JSON-RPC response with isError=true, NOT a transport error), and a
// transport error (distinct from isError).
//
// MCP calls route through this interface:
//
//   - in-process: agentcore's mcpTool.Run → MCPBroker (the localMCPBroker
//     below, wrapping the host-side credentialed *mcp.Client).
//
// Keeping a single interface is what lets the credential boundary — WHO actually
// holds the connector secrets and spawns the MCP subprocesses — move behind the
// seam (e.g. into a separate broker process, issue #167) without touching the
// agent loop or its tool wiring. "In-process" then means only where the loop
// runs, not where the secrets live.
type MCPBroker interface {
	// CallMCP runs server.tool with args and returns the flattened text, the
	// tool-level isError bit, and a transport error (distinct from isError).
	CallMCP(ctx context.Context, server, tool string, args map[string]any) (text string, isError bool, err error)
}

// mcpCaller is the minimal client surface localMCPBroker needs: run a tool on a
// named server. *mcp.Client satisfies it. Depending on the interface (not the
// concrete client) keeps the flatten/isError rendering unit-testable and lets a
// future out-of-process broker (issue #167) substitute an RPC-backed caller
// without touching this code.
type mcpCaller interface {
	CallToolOn(ctx context.Context, server, tool string, args map[string]any) (*mcp.ToolResult, error)
}

// localMCPBroker is the in-process MCPBroker: it runs the call directly against a
// host-side credentialed client. The connector credentials live in that client's
// stdio-server subprocess env (bound host-side via BindMCPSelection / the
// manager's startup wiring) or its HTTP headers; they are applied at THIS call,
// never shipped to the model or into the sandbox.
//
// This is the single home of the call → flatten → fast.io guard/trim → isError
// rendering for the in-process mcpTool. An out-of-process broker (#167) reuses
// the same implementation, so every caller renders an identical result.
type localMCPBroker struct {
	client mcpCaller
	hints  RemediationHints
}

// localMCPBroker is the concrete in-process MCPBroker; assert the contract here,
// in its home package (the in-process loop and the out-of-process broker depend on it).
var _ MCPBroker = (*localMCPBroker)(nil)

// NewLocalMCPBroker returns the in-process MCPBroker that runs calls directly
// against client. hints configures the fast.io inline-base64 upload pre-guard
// (callers that have no specific remediation context pass DefaultRemediationHints).
func NewLocalMCPBroker(client *mcp.Client, hints RemediationHints) MCPBroker {
	return &localMCPBroker{client: client, hints: hints}
}

func (b *localMCPBroker) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	fullName := "mcp_" + server + "_" + tool
	// fast.io inline-base64 pre-guard: reject oversized inline uploads before they
	// hit the wire. A blocked upload is surfaced to the agent as a tool-level error
	// (isError=true) carrying the remediation hint — same shape a real isError takes.
	if ok, hint := rejectFastIOInlineBase64Upload(fullName, args, b.hints); !ok {
		return hint, true, nil
	}
	result, err := b.client.CallToolOn(ctx, server, tool, args)
	if err != nil {
		return "", false, err
	}
	var sb strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
			sb.WriteString("\n")
		}
	}
	resultText := sb.String()
	if server == mcpServerFastIO {
		resultText = trimFastIOResponse(resultText)
	}
	return resultText, result.IsError, nil
}
