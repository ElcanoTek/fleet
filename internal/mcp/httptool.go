package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/itchyny/gojq"
)

// Inline HTTP tools — lightweight REST API tools a client-config bundle declares
// in the manifest's http_tools[] section WITHOUT standing up a full MCP server
// (issue #261). They are registered onto this credentialed *mcp.Client as a single
// SYNTHETIC server (HTTPToolServerName, "_http") so they flow through the SAME
// host-side seam every MCP tool funnels through: discovery via GetAllTools, dispatch
// via CallToolOn/CallToolPrefixed, and the agentcore MCPBroker (policy gate, output
// redaction, isError mapping, critical-tool gate). No new governance path is forked.
//
// SECURITY — the credential boundary is identical to an HTTP MCP server's: the
// tool's secrets arrive as already-resolved request Headers (the caller resolved
// ${ENV_VAR} from the HOST env), so they live only in THIS process (the host-side
// manager, or the out-of-process mcp-broker under issue #167) and are written
// directly onto the outbound request. They are never returned in a tool result,
// never logged here, and never enter the sandbox or the model context. The model
// supplies only the declared input params (substituted into the URL/body) and sees
// only the (downstream-redacted) response body.

// HTTPToolServerName is the synthetic MCP-server name inline HTTP tools register
// under. Mirrors clientconfig.HTTPToolServerName (kept in sync; the two packages
// do not import each other).
const HTTPToolServerName = "_http"

// httpToolClientTimeout bounds a single inline-HTTP-tool request. Per the issue
// spec: 30s, redirects followed (http.DefaultClient's default policy).
const httpToolClientTimeout = 30 * time.Second

// HTTPToolSpec is one inline HTTP tool to register, in the credential-bearing
// runtime shape: Headers already carry their resolved values (the caller expanded
// ${ENV_VAR} host-side). The mcp package keeps its own struct rather than importing
// internal/config so it stays dependency-free; the caller translates.
type HTTPToolSpec struct {
	Name         string
	Description  string
	Method       string // canonical upper-case verb (validated upstream)
	URL          string // may contain {param} tokens
	Headers      map[string]string
	BodyTemplate string // may contain {param} tokens; sent for POST/PUT/PATCH
	InputSchema  map[string]interface{}
	ResponseJQ   string // optional jq program over a JSON response body
}

// AddHTTPTools registers each spec as a tool on the synthetic HTTPToolServerName
// server. It is additive and idempotent at the server level: repeated calls append
// to the same synthetic server's tool list (a re-load in a single session must not
// drop earlier tools). A spec with an empty name is skipped. No network call is
// made here — registration is local; the request runs only when the tool is called.
func (c *Client) AddHTTPTools(specs []HTTPToolSpec) {
	if len(specs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	server, ok := c.servers[HTTPToolServerName]
	if !ok {
		server = &Server{
			name:      HTTPToolServerName,
			transport: &httpToolTransport{tools: map[string]HTTPToolSpec{}},
		}
		c.servers[HTTPToolServerName] = server
	}
	tr, ok := server.transport.(*httpToolTransport)
	if !ok {
		// The synthetic server name is reserved for this transport; if something
		// else claimed it, refuse to corrupt it.
		return
	}
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}
		schema := spec.InputSchema
		if schema == nil {
			// Advertise a well-formed empty object schema for a no-parameter tool so
			// the model is handed a valid JSON Schema.
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		tr.tools[name] = spec
		server.tools = append(server.tools, Tool{
			Name:        name,
			Description: spec.Description,
			InputSchema: schema,
		})
	}
}

// httpToolTransport implements Transport for the synthetic "_http" server. It does
// NOT speak JSON-RPC over a pipe/socket: it answers the MCP methods the client's
// Server.initialize + Server.callTool path issues (initialize / tools/list are
// never routed here because AddHTTPTools populates Server.tools directly and does
// not call initialize; tools/call is the live path) by executing the named tool's
// REST request in-process. Keeping it behind the Transport interface is what lets
// these tools reuse Server.callTool, the broker, and the whole governance stack
// unchanged.
type httpToolTransport struct {
	tools  map[string]HTTPToolSpec
	client *http.Client
}

// Call handles the one method the registration path drives at runtime: tools/call.
// Other methods are not used (initialize/tools/list are short-circuited by
// AddHTTPTools writing Server.tools directly), so they return an explicit error
// rather than silently succeeding.
func (t *httpToolTransport) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if method != "tools/call" {
		return nil, fmt.Errorf("http tool transport: unsupported method %q", method)
	}
	p, ok := params.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("http tool transport: malformed tools/call params")
	}
	name, _ := p[jsonRPCFieldName].(string)
	spec, ok := t.tools[name]
	if !ok {
		return nil, fmt.Errorf("http tool not found: %s", name)
	}
	args, _ := p["arguments"].(map[string]interface{})

	result, err := executeHTTPTool(ctx, t.httpClient(), spec, args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// Notify is a no-op: inline HTTP tools have no JSON-RPC lifecycle.
func (t *httpToolTransport) Notify(context.Context, string, interface{}) error { return nil }

// Close is a no-op: there is no subprocess or socket to tear down.
func (t *httpToolTransport) Close() error { return nil }

// httpClient lazily builds the bounded client (30s timeout, default redirect
// policy) the first time a tool is invoked.
func (t *httpToolTransport) httpClient() *http.Client {
	if t.client == nil {
		t.client = &http.Client{Timeout: httpToolClientTimeout}
	}
	return t.client
}

// executeHTTPTool runs one inline HTTP tool and returns it as a ToolResult. The
// flow: substitute {param} tokens into the URL (percent-encoded) and body (raw),
// apply the resolved auth/static headers, execute with the bounded client, then
// render the response. A non-2xx is NOT a transport error — it is returned to the
// model as a "status <N>: <body>" text result (isError) so the model can reason
// about the failure (matching the issue's acceptance criteria). When response_jq
// is set and the body is valid JSON, the jq program filters/transforms it first.
func executeHTTPTool(ctx context.Context, client *http.Client, spec HTTPToolSpec, args map[string]interface{}) (*ToolResult, error) {
	reqURL := substituteTokens(spec.URL, args, true)

	var body io.Reader
	if spec.BodyTemplate != "" {
		body = strings.NewReader(substituteTokens(spec.BodyTemplate, args, false))
	}

	req, err := http.NewRequestWithContext(ctx, spec.Method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	out := string(respBody)
	// Apply the jq filter only when the body is valid JSON; a non-JSON body (an
	// HTML error page, plain text) is passed through verbatim. The jq program was
	// syntax-checked at Load (clientconfig.validateHTTPTools), so a parse failure
	// here is unexpected and is reported rather than silently dropping the filter.
	if jqProgram := strings.TrimSpace(spec.ResponseJQ); jqProgram != "" {
		if filtered, ok, jqErr := applyResponseJQ(jqProgram, respBody); jqErr != nil {
			return nil, jqErr
		} else if ok {
			out = filtered
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface the failure to the model (isError) instead of erroring the call,
		// so it can reason about and recover from a 4xx/5xx.
		return &ToolResult{
			Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf("status %d: %s", resp.StatusCode, out)}},
			IsError: true,
		}, nil
	}

	return &ToolResult{Content: []ContentBlock{{Type: "text", Text: out}}}, nil
}

// applyResponseJQ runs program over body when body is valid JSON. ok is false (no
// error) when the body is not JSON — the caller then passes the raw body through.
// Multiple jq outputs are newline-joined; scalars/objects are rendered as compact
// JSON.
func applyResponseJQ(program string, body []byte) (out string, ok bool, err error) {
	var input interface{}
	if jsonErr := json.Unmarshal(body, &input); jsonErr != nil {
		//nolint:nilerr // intentional: a non-JSON body is not an error — ok=false signals "pass the raw body through unfiltered" (response_jq applies only to JSON, per the issue spec).
		return "", false, nil
	}
	q, parseErr := gojq.Parse(program)
	if parseErr != nil {
		return "", false, fmt.Errorf("response_jq parse: %w", parseErr)
	}
	var results []string
	iter := q.Run(input)
	for {
		v, more := iter.Next()
		if !more {
			break
		}
		if e, isErr := v.(error); isErr {
			return "", false, fmt.Errorf("response_jq eval: %w", e)
		}
		switch s := v.(type) {
		case string:
			results = append(results, s)
		default:
			b, mErr := json.Marshal(v)
			if mErr != nil {
				return "", false, fmt.Errorf("response_jq render: %w", mErr)
			}
			results = append(results, string(b))
		}
	}
	return strings.Join(results, "\n"), true, nil
}

// substituteTokens replaces {param} tokens in tmpl with args[param]. In URL
// context (urlEncode=true) the value is percent-encoded (query-escaped) so a param
// can't inject path/query structure; in body context the value is inserted raw (the
// template author controls the surrounding JSON quoting). Non-string arg values are
// rendered with %v.
//
// A "{...}" run is treated as a token ONLY when its content is a valid token name
// (ASCII letters, digits, underscore). This is what keeps a JSON body_template safe:
// the literal braces of `{"channel":"{channel}"}` are not token names, so only the
// real {channel} placeholder is substituted. A valid-looking token with no matching
// arg is left intact rather than blanked, so a stray "{foo}" survives.
func substituteTokens(tmpl string, args map[string]interface{}, urlEncode bool) string {
	if !strings.Contains(tmpl, "{") {
		return tmpl
	}
	var sb strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			sb.WriteByte(tmpl[i])
			i++
			continue
		}
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			// No closing brace: emit the rest verbatim.
			sb.WriteString(tmpl[i:])
			break
		}
		key := tmpl[i+1 : i+end]
		if v, present := args[key]; present && isTokenName(key) {
			s := fmt.Sprintf("%v", v)
			if urlEncode {
				s = url.QueryEscape(s)
			}
			sb.WriteString(s)
			i += end + 1
			continue
		}
		// Not a substitutable token (literal brace, or a token with no arg): emit
		// the opening '{' and resume scanning AFTER it so a nested real token like
		// the inner {channel} in `{"x":"{channel}"}` is still found.
		sb.WriteByte('{')
		i++
	}
	return sb.String()
}

// isTokenName reports whether s is a non-empty run of ASCII letters, digits, and
// underscores — the shape a {param} placeholder must have to be substituted. This
// is what disambiguates a real placeholder from a JSON object's literal braces.
func isTokenName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}
