package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	tools   []string // tool names advertised on the first request
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
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &req) == nil {
		c.mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "system" || m.Role == "developer" {
				c.prompts = append(c.prompts, rawContentText(m.Content))
			}
		}
		if c.tools == nil {
			for _, t := range req.Tools {
				c.tools = append(c.tools, t.Function.Name)
			}
		}
		c.mu.Unlock()
	}
	c.next.ServeHTTP(w, r)
}

func (c *systemPromptCapture) systemPrompts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.prompts...)
}

func (c *systemPromptCapture) toolNames() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := map[string]bool{}
	for _, n := range c.tools {
		m[n] = true
	}
	return m
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
// #1006: the prompt the model receives must list the hosted tools it is
// offered — the section is appended by agentcore.Run from the built roster —
// and name the connection the overlay could not mount (the builder's notice).
// Only a whole-turn test sees the assembled prompt; the builder-level tests
// cannot.
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

// TestManagerRunTurn_SystemPromptDescribesDeferredTools: above the disclosure
// threshold the MCP tools are hidden behind tool_search/tool_describe/tool_call
// (#506), and the prompt must say so instead of listing names the model cannot
// call. Four hosted connectors (159 tools) did exactly that during the #1006
// verification: the section said "Call exactly these names", the model called
// one, the framework answered "tool not found", and the model concluded the
// connector was down without ever trying the bridges.
func TestManagerRunTurn_SystemPromptDescribesDeferredTools(t *testing.T) {
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep("noted")}})
	capture := &systemPromptCapture{next: fake.Handler()}

	const n = 130 // + the native tools > the default 128 threshold
	catalog := make([]mcp.ServerTool, 0, n)
	for i := 0; i < n; i++ {
		catalog = append(catalog, mcp.ServerTool{ServerName: "big", Tool: mcp.Tool{
			Name:        fmt.Sprintf("tool_%03d", i),
			Description: "a deferred tool",
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}})
	}
	mgr := newFakeLLMManagerWithHandler(t, capture, func(opts *ManagerOptions) {
		opts.OpenRemoteMCPOverlay = func(context.Context, string, map[string]bool, RemoteMCPSelection) (*RemoteMCPOverlay, error) {
			return &RemoteMCPOverlay{
				Broker:     inertMCPBroker{},
				Servers:    map[string]bool{"big": true},
				Catalog:    catalog,
				CloseScope: func(context.Context) error { return nil },
			}, nil
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := mgr.RunTurn(ctx, TurnInput{
		UserMessage: "use a big tool",
		Model:       "anthropic/claude-opus-4.8",
		UserEmail:   "user@example.test",
	}, &recordingSink{}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	tools := capture.toolNames()
	if !tools["tool_search"] || !tools["tool_call"] || tools["mcp_big_tool_007"] {
		t.Fatalf("expected a deferred roster (bridges present, mcp_big_* absent); got %d tools, tool_search=%v mcp_big_tool_007=%v", len(tools), tools["tool_search"], tools["mcp_big_tool_007"])
	}
	prompts := capture.systemPrompts()
	if len(prompts) == 0 {
		t.Fatal("the fake LLM received no system prompt")
	}
	p := prompts[0]
	for _, want := range []string{"## MCP Tools (live registry)", "130 MCP tools are available this turn", "NOT in your tool list by name", "`tool_call {name, arguments}`", "- `big` (130 tools)"} {
		if !strings.Contains(p, want) {
			t.Errorf("deferred-mode prompt missing %q\n--- prompt ---\n%s", want, p)
		}
	}
	for _, banned := range []string{"Call exactly these names", "- `mcp_big_tool_007`", "No MCP tools are currently connected"} {
		if strings.Contains(p, banned) {
			t.Errorf("deferred-mode prompt still says %q — the model would call a name that is not registered\n--- prompt ---\n%s", banned, p)
		}
	}
}
