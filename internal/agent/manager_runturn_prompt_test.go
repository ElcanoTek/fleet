package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/fakellm"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// systemPromptCapture records the system prompt of every chat-completions
// request on its way to the fake LLM, so a RunTurn test can assert what the
// model was actually told — the prompt is assembled inside RunTurn and never
// returned to the caller.
type systemPromptCapture struct {
	mu      sync.Mutex
	prompts []string
	next    http.Handler
}

func (c *systemPromptCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) == nil {
		for _, m := range req.Messages {
			if m.Role == "system" || m.Role == "developer" {
				c.mu.Lock()
				c.prompts = append(c.prompts, rawContentText(m.Content))
				c.mu.Unlock()
			}
		}
	}
	c.next.ServeHTTP(w, r)
}

func (c *systemPromptCapture) systemPrompts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.prompts...)
}

// rawContentText flattens a chat-completions content field — a plain string
// or an array of typed parts — into text.
func rawContentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

// TestManagerRunTurn_SystemPromptNamesHostedTools is the RunTurn-level net for
// the ordering fix behind #1006: the per-user hosted overlay must be open
// before the system prompt is composed, so the prompt's live-registry section
// lists the hosted tools the model is offered instead of denying them, and
// names the connection the overlay could not mount. Only a whole-turn test can
// catch a future edit that moves composeTurnSystemPrompt back above the
// overlay — the builder-level tests would still pass.
func TestManagerRunTurn_SystemPromptNamesHostedTools(t *testing.T) {
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep("noted")}})
	capture := &systemPromptCapture{next: fake.Handler()}

	mgr := newFakeLLMManagerWithHandler(t, capture, func(opts *ManagerOptions) {
		opts.OpenRemoteMCPOverlay = func(context.Context, string, map[string]bool, RemoteMCPSelection) (*RemoteMCPOverlay, error) {
			return &RemoteMCPOverlay{
				Broker:  inertMCPBroker{},
				Servers: map[string]bool{"github_work": true},
				Catalog: []mcp.ServerTool{{ServerName: "github_work", Tool: mcp.Tool{
					Name:        "get_me",
					Description: "Get the authenticated user.",
					InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
				}}},
				Skipped:    []string{"github_personal"},
				CloseScope: func(context.Context) error { return nil },
			}, nil
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := mgr.RunTurn(ctx, TurnInput{
		UserMessage: "which github login am I?",
		Model:       "anthropic/claude-opus-4.8",
		UserEmail:   "user@example.test", // arms the overlay
	}, &recordingSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	prompts := capture.systemPrompts()
	if len(prompts) == 0 {
		t.Fatal("the fake LLM received no system prompt")
	}
	p := prompts[0]
	if strings.Contains(p, "No MCP tools are currently connected") {
		t.Errorf("system prompt denies MCP tools while the hosted overlay mounted github_work\n--- prompt ---\n%s", p)
	}
	if !strings.Contains(p, "- `mcp_github_work_get_me`") {
		t.Errorf("system prompt does not advertise the hosted tool the model was offered\n--- prompt ---\n%s", p)
	}
	if !strings.Contains(p, "`github_personal`") || !strings.Contains(p, "could NOT be mounted") {
		t.Errorf("system prompt does not name the connection the overlay skipped\n--- prompt ---\n%s", p)
	}
}
