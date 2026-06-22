package acpruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// toolForwarder is the agent-side sandbox.Delegate: it ships every bash/python
// invocation over the ACP `_fleet/tool` extension to the client, which runs it
// in the host-managed sandbox and returns the result. This is what makes the
// agent unable to self-execute — it has no local container, only this forwarder.
type toolForwarder struct {
	conn      *acp.AgentSideConnection
	sessionID string
}

var _ sandbox.Delegate = (*toolForwarder)(nil)

func (f *toolForwarder) RunBash(ctx context.Context, req sandbox.BashRequest) (sandbox.BashResult, error) {
	resp, err := f.call(ctx, ToolRequest{
		SessionID: f.sessionID, Tool: ToolBash,
		Command: req.Command, WorkingDir: req.WorkingDir,
		TimeoutSeconds: secs(req.Timeout),
	})
	if err != nil {
		return sandbox.BashResult{}, err
	}
	// Map the delegated result back onto a BashResult: Output is the combined
	// view; a tool failure rides Error → surfaced via a non-zero exit so the
	// tool layer renders it exactly as the in-process path would.
	res := sandbox.BashResult{Stdout: []byte(resp.Output), ExitCode: resp.ExitCode, TimedOut: resp.TimedOut}
	if resp.Error != "" && resp.ExitCode == 0 {
		res.ExitCode = 1
		res.Stderr = []byte(resp.Error)
	}
	return res, nil
}

func (f *toolForwarder) RunPython(ctx context.Context, req sandbox.PythonRequest) (sandbox.PythonResult, error) {
	resp, err := f.call(ctx, ToolRequest{
		SessionID: f.sessionID, Tool: ToolPython,
		Code: req.Code, WorkspaceDir: req.WorkspaceDir,
		TimeoutSeconds: secs(req.Timeout),
	})
	if err != nil {
		return sandbox.PythonResult{}, err
	}
	out := sandbox.PythonResult{Status: "ok", Output: resp.Output, Stdout: resp.Output}
	if resp.Error != "" {
		out.Status = "error"
		out.Error = resp.Error
	}
	return out, nil
}

func (f *toolForwarder) call(ctx context.Context, req ToolRequest) (ToolResponse, error) {
	raw, err := f.conn.CallExtension(ctx, ExtMethodTool, req)
	if err != nil {
		return ToolResponse{}, fmt.Errorf("%s: %w", ExtMethodTool, err)
	}
	var resp ToolResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ToolResponse{}, fmt.Errorf("decode tool response: %w", err)
	}
	return resp, nil
}

// delegatingObserver maps agentcore run events onto ACP session/update (the
// user-visible streaming surface) and `_fleet/event` (the structured governance
// surface). Text deltas stream as agent_message chunks (per-chunk flush so SSE
// streams on the client); other events ride _fleet/event so the host Observer
// sees the same (eventType, payload) stream the in-process path emits.
type delegatingObserver struct {
	conn      *acp.AgentSideConnection
	sessionID acp.SessionId
}

var _ interface {
	Observe(string, map[string]any)
} = (*delegatingObserver)(nil)

func (o *delegatingObserver) Observe(eventType string, payload map[string]any) {
	ctx := context.Background()
	switch eventType {
	case "text.delta", "text":
		if text, _ := payload["text"].(string); text != "" {
			// Per-chunk flush: one SessionUpdate per delta → SSE streams live.
			_ = o.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: o.sessionID,
				Update:    acp.UpdateAgentMessageText(text),
			})
			return
		}
	case "reasoning.delta", "reasoning":
		if text, _ := payload["text"].(string); text != "" {
			_ = o.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: o.sessionID,
				Update:    acp.UpdateAgentThoughtText(text),
			})
			return
		}
	case "tool.call":
		title, _ := payload["name"].(string)
		if title == "" {
			title, _ = payload["title"].(string)
		}
		id, _ := payload["id"].(string)
		if id == "" {
			id = newSessionID()
		}
		_ = o.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: o.sessionID,
			Update:    acp.StartToolCall(acp.ToolCallId(id), title),
		})
	}
	// Everything (including the cases above) ALSO rides _fleet/event so the host
	// Observer receives the full neutral event stream for governance/persistence.
	_ = o.conn.NotifyExtension(ctx, ExtMethodEvent, EventNotification{
		SessionID: string(o.sessionID),
		EventType: eventType,
		Payload:   payload,
	})
}

// --- decode helpers (agent side) ---

func decodeRunSpec(meta map[string]any) (RunSpec, error) {
	var spec RunSpec
	raw, err := rawFromMeta(meta, MetaKeyRunSpec)
	if err != nil {
		return spec, err
	}
	if raw == nil {
		return spec, fmt.Errorf("missing %s in session _meta", MetaKeyRunSpec)
	}
	return spec, json.Unmarshal(raw, &spec)
}

func decodePromptMeta(meta map[string]any) (PromptMeta, error) {
	var pm PromptMeta
	raw, err := rawFromMeta(meta, MetaKeyPromptMeta)
	if err != nil {
		return pm, err
	}
	if raw == nil {
		return pm, nil // no replayed history; the ACP prompt blocks stand alone
	}
	return pm, json.Unmarshal(raw, &pm)
}

// rawFromMeta extracts a key from an ACP _meta map as raw JSON. The value may
// arrive as json.RawMessage (same process) or decoded into any (over the wire).
func rawFromMeta(meta map[string]any, key string) (json.RawMessage, error) {
	v, ok := meta[key]
	if !ok {
		return nil, nil
	}
	switch t := v.(type) {
	case json.RawMessage:
		return t, nil
	case string:
		return json.RawMessage(t), nil
	default:
		return json.Marshal(t)
	}
}

// decodeMessages rebuilds the fantasy message slice. When the client shipped a
// replayed history (MessagesJSON), it is authoritative. Otherwise fall back to
// the ACP prompt content blocks (text only).
func decodeMessages(messagesJSON string, prompt []acp.ContentBlock) ([]fantasy.Message, error) {
	if messagesJSON != "" {
		var msgs []fantasy.Message
		if err := json.Unmarshal([]byte(messagesJSON), &msgs); err != nil {
			return nil, err
		}
		return msgs, nil
	}
	var text string
	for _, b := range prompt {
		if b.Text != nil {
			text += b.Text.Text
		}
	}
	return []fantasy.Message{fantasy.NewUserMessage(text)}, nil
}

func newSessionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "acp-" + hex.EncodeToString(b[:])
}

func secs(d time.Duration) int {
	return int(d.Seconds())
}
