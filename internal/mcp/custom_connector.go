package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
)

// AddHTTPToolsOnServer registers inline HTTP tools under a named server (for
// example a user-owned or shared custom "http_tools" connector in
// remote_mcp_servers) so they appear as mcp_<serverName>_<toolName> and flow
// through the exact same MCP governance, policy, and secret-redaction pipeline.
func (c *Client) AddHTTPToolsOnServer(serverName string, specs []HTTPToolSpec, httpClient ...*http.Client) error {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return fmt.Errorf("server name is required")
	}
	if len(specs) == 0 {
		return fmt.Errorf("at least one HTTP tool spec is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, exists := c.servers[serverName]; exists && existing != nil {
		_ = existing.transport.Close()
	}

	tr := &httpToolTransport{tools: make(map[string]HTTPToolSpec, len(specs))}
	if len(httpClient) > 0 && httpClient[0] != nil {
		// Preserve newHTTPToolClient's redirect policy (follow redirects while
		// stripping credential headers on cross-origin hops) even when the caller
		// passes an httpClient configured with a custom DialContext.
		dial := httpToolDialContext
		if ht, ok := httpClient[0].Transport.(*http.Transport); ok && ht != nil && ht.DialContext != nil {
			dial = ht.DialContext
		}
		tr.client = newHTTPToolClient(dial)
	}
	server := &Server{
		name:      serverName,
		transport: tr,
	}
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}
		if spec.Method == "" {
			spec.Method = "GET"
		} else {
			spec.Method = strings.ToUpper(strings.TrimSpace(spec.Method))
		}
		schema := spec.InputSchema
		if schema == nil {
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		tr.tools[name] = spec
		server.tools = append(server.tools, Tool{
			Name:        name,
			Description: spec.Description,
			InputSchema: schema,
			Critical:    spec.Method != "GET",
		})
	}
	if len(server.tools) == 0 {
		return fmt.Errorf("no valid HTTP tools in connector %q", serverName)
	}
	c.servers[serverName] = server
	return nil
}

// PlaywrightToolSpec defines a single declarative Playwright browser tool inside
// a custom "playwright" connector. Credentials are already resolved host-side.
type PlaywrightToolSpec struct {
	Name        string
	Description string
	URL         string
	Steps       []playwrightrunner.Step
	Credentials playwrightrunner.Credentials
	InputSchema map[string]interface{}
}

// AddPlaywrightToolsOnServer registers declarative Playwright browser tools
// under a named server so they appear as mcp_<serverName>_<toolName>.
func (c *Client) AddPlaywrightToolsOnServer(serverName string, specs []PlaywrightToolSpec) error {
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return fmt.Errorf("server name is required")
	}
	if len(specs) == 0 {
		return fmt.Errorf("at least one Playwright tool spec is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, exists := c.servers[serverName]; exists && existing != nil {
		_ = existing.transport.Close()
	}

	tr := &playwrightToolTransport{tools: make(map[string]PlaywrightToolSpec, len(specs))}
	server := &Server{
		name:      serverName,
		transport: tr,
	}
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}
		schema := spec.InputSchema
		if schema == nil {
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		tr.tools[name] = spec
		server.tools = append(server.tools, Tool{
			Name:        name,
			Description: spec.Description,
			InputSchema: schema,
			Critical:    isPlaywrightToolSpecCritical(spec),
		})
	}
	if len(server.tools) == 0 {
		return fmt.Errorf("no valid Playwright tools in connector %q", serverName)
	}
	c.servers[serverName] = server
	return nil
}

func isPlaywrightToolSpecCritical(spec PlaywrightToolSpec) bool {
	for _, st := range spec.Steps {
		switch strings.ToLower(strings.TrimSpace(st.Action)) {
		case "fill", "click", "press", "select":
			return true
		}
	}
	lower := strings.ToLower(strings.TrimSpace(spec.Name))
	for _, verb := range []string{"create", "update", "delete", "remove", "destroy", "submit", "write"} {
		if lower == verb || strings.HasPrefix(lower, verb+"_") || strings.HasSuffix(lower, "_"+verb) {
			return true
		}
	}
	return false
}

type playwrightToolTransport struct {
	mu    sync.RWMutex
	tools map[string]PlaywrightToolSpec
}

func (t *playwrightToolTransport) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if method != "tools/call" {
		return nil, fmt.Errorf("playwright tool transport: unsupported method %q", method)
	}
	p, ok := params.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("playwright tool transport: malformed tools/call params")
	}
	name, _ := p[jsonRPCFieldName].(string)
	t.mu.RLock()
	spec, ok := t.tools[name]
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("playwright tool not found: %s", name)
	}
	args, _ := p["arguments"].(map[string]interface{})

	runRes, err := playwrightrunner.Run(ctx, playwrightrunner.RunRequest{
		URL:         spec.URL,
		Steps:       spec.Steps,
		Credentials: spec.Credentials,
		Args:        args,
	})
	if err != nil {
		return json.Marshal(&ToolResult{
			Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf("playwright browser error: %v", err)}},
			IsError: true,
		})
	}
	outBytes, err := json.MarshalIndent(runRes, "", "  ")
	if err != nil {
		return nil, err
	}
	return json.Marshal(&ToolResult{
		Content: []ContentBlock{{Type: "text", Text: string(outBytes)}},
	})
}

func (t *playwrightToolTransport) Notify(context.Context, string, interface{}) error { return nil }
func (t *playwrightToolTransport) Close() error                                      { return nil }
