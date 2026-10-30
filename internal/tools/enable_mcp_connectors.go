package tools

import (
	"context"
	"encoding/json"
	"strings"

	"charm.land/fantasy"
)

// EnableMCPConnectorsToolName is the canonical name of the interactive tool
// that enables connected optional MCP connectors for the current conversation
// (either immediately when auto-approve is on, or via a user approval card).
const EnableMCPConnectorsToolName = "enable_mcp_connectors"

// EnableMCPConnectorsParams is the schema-generating input struct for
// enable_mcp_connectors.
type EnableMCPConnectorsParams struct {
	Connectors []string `json:"connectors" description:"Names of connected optional MCP connectors to enable for this conversation (from the 'Available Optional MCP Connectors' section of your system prompt, e.g. ['google_sheets'] or ['hubspot', 'slack'])."`
	Reason     string   `json:"reason,omitempty" description:"Short, user-facing explanation of why these connectors are needed for the user's request (shown on the approval card when auto-approve is off)."`
}

// EnableMCPConnectorsResult is the structured result returned by an
// EnableMCPConnectorsHandler.
type EnableMCPConnectorsResult struct {
	Status          string   `json:"status"`
	Connectors      []string `json:"connectors,omitempty"`
	EnabledOptional []string `json:"enabled_optional,omitempty"`
	AutoApproved    bool     `json:"auto_approved,omitempty"`
	ApprovalID      string   `json:"approval_id,omitempty"`
	Message         string   `json:"message"`
	StopTurn        bool     `json:"-"`
}

// EnableMCPConnectorsHandler validates and either immediately enables or stages
// an approval card for the requested optional MCP connectors. Injected onto the
// turn context by httpapi.
type EnableMCPConnectorsHandler func(ctx context.Context, toolCallID string, params EnableMCPConnectorsParams) (*EnableMCPConnectorsResult, error)

type enableMCPConnectorsHandlerKey struct{}

// WithEnableMCPConnectorsHandler attaches a per-turn EnableMCPConnectorsHandler
// to ctx.
func WithEnableMCPConnectorsHandler(ctx context.Context, h EnableMCPConnectorsHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, enableMCPConnectorsHandlerKey{}, h)
}

func enableMCPConnectorsHandlerFromContext(ctx context.Context) EnableMCPConnectorsHandler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(enableMCPConnectorsHandlerKey{}).(EnableMCPConnectorsHandler)
	return h
}

const enableMCPConnectorsDescription = `Enable one or more connected Optional MCP connectors for THIS conversation so their tools become available.

Check the "Available Optional MCP Connectors (not currently enabled)" section in your system prompt before searching for tools or attempting bash/python workarounds. When a user's request requires an optional MCP connector listed there:
1. Call enable_mcp_connectors with the connector name(s) and a clear one-sentence reason.
2. Only request connectors that are listed as available/connected.
3. If auto-approve is enabled for this conversation (or during an unattended patrol turn), the connector(s) are enabled immediately and your turn automatically resumes with their tools mounted.
4. If auto-approve is off, an approval card is shown to the user; explain briefly which connector(s) you suggested and wait — as soon as the user clicks Approve, your turn automatically resumes with those connectors enabled.`

// NewEnableMCPConnectorsTool constructs the interactive enable_mcp_connectors tool.
func NewEnableMCPConnectorsTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		EnableMCPConnectorsToolName,
		enableMCPConnectorsDescription,
		func(ctx context.Context, params EnableMCPConnectorsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			cleaned := make([]string, 0, len(params.Connectors))
			seen := make(map[string]bool, len(params.Connectors))
			for _, raw := range params.Connectors {
				name := strings.ToLower(strings.TrimSpace(raw))
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				cleaned = append(cleaned, name)
			}
			if len(cleaned) == 0 {
				return fantasy.NewTextErrorResponse(
					"connectors is required: pass one or more connector names from the Available Optional MCP Connectors list",
				), nil
			}
			params.Connectors = cleaned
			params.Reason = strings.TrimSpace(params.Reason)

			h := enableMCPConnectorsHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse(
					"enable_mcp_connectors is only available in interactive chat conversations",
				), nil
			}

			res, err := h(ctx, call.ID, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			text := res.Message
			if text == "" {
				if out, mErr := json.MarshalIndent(res, "", "  "); mErr == nil {
					text = string(out)
				}
			}
			resp := fantasy.NewTextResponse(text)
			if res.StopTurn {
				resp.StopTurn = true
			}
			return resp, nil
		},
	)
}
