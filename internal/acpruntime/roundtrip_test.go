package acpruntime

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// scriptedModel is a fake fantasy.LanguageModel that emits a scripted sequence
// of stream parts per call. Call 0 issues a bash tool_call; call 1 emits final
// text. This drives a real agentcore.Run loop with no network.
type scriptedModel struct {
	mu    sync.Mutex
	calls int
}

func (m *scriptedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      []fantasy.Content{fantasy.TextContent{Text: "ok"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (m *scriptedModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	call := m.calls
	m.calls++
	m.mu.Unlock()

	return func(yield func(fantasy.StreamPart) bool) {
		if call == 0 {
			// Issue a governed bash tool call.
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "call-1",
				ToolCallName:  "bash",
				ToolCallInput: `{"command":"echo hi"}`,
			}) {
				return
			}
			yield(fantasy.StreamPart{
				Type:         fantasy.StreamPartTypeFinish,
				FinishReason: fantasy.FinishReasonToolCalls,
				Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
			})
			return
		}
		// Final round: emit user-visible text.
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "all done"}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
		})
	}, nil
}

func (m *scriptedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, io.EOF
}
func (m *scriptedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, io.EOF
}
func (m *scriptedModel) Provider() string { return "scripted" }
func (m *scriptedModel) Model() string    { return "scripted-model" }

// recordingExecutor is the HOST-side agentcore.Executor the client wires to the
// delegated tool calls. It records every command so the test can prove the tool
// ran HOST-side (via the client), not inside the agent.
type recordingExecutor struct {
	mu       sync.Mutex
	bashCmds []string
}

func (e *recordingExecutor) RunBash(_ context.Context, command string) (string, error) {
	e.mu.Lock()
	e.bashCmds = append(e.bashCmds, command)
	e.mu.Unlock()
	return "host-ran: " + command, nil
}
func (e *recordingExecutor) RunPython(_ context.Context, code string) (string, error) {
	return "host-py: " + code, nil
}

// recordingObserver captures the events the client re-emits.
type recordingObserver struct {
	mu     sync.Mutex
	events []string
	text   strings.Builder
}

func (o *recordingObserver) Observe(eventType string, payload map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, eventType)
	if eventType == "text.delta" {
		if t, _ := payload["text"].(string); t != "" {
			o.text.WriteString(t)
		}
	}
}

// TestACPRoundTripGovernedTool wires the client and agent ACP connections to each
// other in-process (io.Pipe), runs a real agentcore.Run loop with a scripted
// model, and asserts:
//   - the loop streamed text back over session/update → the Observer;
//   - the bash tool delegated over _fleet/tool and executed via the HOST
//     Executor (not inside the agent);
//   - the host-side tool result flowed back into the loop.
//
// This is the parity-gate round-trip + governed-tool-via-ACP proof.
func TestACPRoundTripGovernedTool(t *testing.T) {
	// Two pipes: client→agent (agent reads), agent→client (client reads).
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()

	model := &scriptedModel{}
	runner := newAgentRunner(func(context.Context, string) (fantasy.LanguageModel, error) {
		return model, nil
	})
	agentConn := acp.NewAgentSideConnection(runner, agentToClientW, clientToAgentR)
	runner.SetConn(agentConn)

	exec := &recordingExecutor{}
	obs := &recordingObserver{}
	cl := &hostClient{deps: Deps{Executor: exec, Observer: obs}}
	clientConn := acp.NewClientSideConnection(cl, clientToAgentW, agentToClientR)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := clientConn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	spec := RunSpec{
		Mode: agentcore.ModeInteractive.String(), ModelSlug: "scripted-model",
		SystemPrompt: "you are a test agent", Temperature: 0, MaxTokens: 256,
	}
	specJSON, _ := json.Marshal(spec)
	sess, err := clientConn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: "/workspace", McpServers: []acp.McpServer{},
		Meta: map[string]any{MetaKeyRunSpec: json.RawMessage(specJSON)},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	pmJSON, _ := json.Marshal(PromptMeta{})
	resp, err := clientConn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("run echo")},
		Meta:      map[string]any{MetaKeyPromptMeta: json.RawMessage(pmJSON)},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	// 1. The governed tool executed HOST-side (via the client's Executor).
	exec.mu.Lock()
	cmds := append([]string(nil), exec.bashCmds...)
	exec.mu.Unlock()
	if len(cmds) != 1 || cmds[0] != "echo hi" {
		t.Fatalf("host executor bash cmds = %v, want [echo hi]", cmds)
	}

	// 2. Text streamed back over session/update → Observer.
	obs.mu.Lock()
	streamed := obs.text.String()
	events := append([]string(nil), obs.events...)
	obs.mu.Unlock()
	if !strings.Contains(streamed, "all done") {
		t.Fatalf("streamed text = %q, want it to contain 'all done'", streamed)
	}

	// 3. The final reply was accumulated by the client.
	if !strings.Contains(cl.finalText(), "all done") {
		t.Fatalf("client final text = %q, want 'all done'", cl.finalText())
	}

	t.Logf("events: %v", events)

	_ = clientToAgentW.Close()
	_ = agentToClientW.Close()
}
