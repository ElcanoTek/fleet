package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMCPHTTPTimeout is the default timeout for HTTP requests to MCP servers
	DefaultMCPHTTPTimeout = 2 * time.Minute

	// nullString is the string representation of a JSON null
	nullString = "null"

	// mcpProtocolVersion is the Model Context Protocol revision fleet
	// announces during the `initialize` handshake. Every fast.io/SSP/email
	// MCP server in this repo speaks 2024-11-05; keep this pinned until
	// the entire fleet supports a newer revision.
	mcpProtocolVersion = "2024-11-05"

	// jsonRPCVersion is the JSON-RPC envelope version every transport
	// emits. Defined once so the stdio call path, the HTTP call path, and
	// future transports stay in lockstep.
	jsonRPCVersion = "2.0"

	// jsonRPCFieldName is the JSON object key MCP and JSON-RPC payloads
	// use for the "name" field. Hoisted because the literal repeats
	// across the initialize handshake and the tools/call payload.
	jsonRPCFieldName = "name"

	// jsonRPCFieldJSONRPC is the envelope's `jsonrpc` field name used by
	// every JSON-RPC request map this package emits.
	jsonRPCFieldJSONRPC = "jsonrpc"

	// jsonRPCFieldProtocolVersion is the `protocolVersion` key used in
	// MCP `initialize` request and response payloads.
	jsonRPCFieldProtocolVersion = "protocolVersion"
)

// Client represents an MCP client that can connect to servers via stdio or HTTP
type Client struct {
	servers map[string]*Server
	mu      sync.RWMutex
}

// Server represents a connection to an MCP server
type Server struct {
	mu        sync.Mutex // protects transport during restart
	name      string
	transport Transport
	tools     []Tool

	// Restart state for stdio servers (nil for HTTP servers).
	stdioCommand string
	stdioArgs    []string
	stdioEnv     map[string]string
	stdioDir     string // cwd the subprocess launches in (bundle root); "" = inherit
}

// Transport interface for different MCP connection types
type Transport interface {
	Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	Close() error
}

// Tool represents an MCP tool
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// ToolResult represents the result of a tool call
type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// RPCError represents a JSON-RPC error that can be unmarshaled from either
// a string or an object with code/message fields.
// Some MCP servers (e.g., Adverity) return errors as plain strings like "Unauthorized"
// instead of the standard {code, message} object format.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Raw     string `json:"-"` // Original JSON for debugging
}

// UnmarshalJSON implements custom unmarshaling to handle both string and object error formats.
func (e *RPCError) UnmarshalJSON(data []byte) error {
	// Store raw JSON for debugging
	e.Raw = string(data)

	// Try to unmarshal as object first (standard JSON-RPC format)
	var objError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &objError); err == nil && (objError.Code != 0 || objError.Message != "") {
		e.Code = objError.Code
		e.Message = objError.Message
		return nil
	}

	// Try to unmarshal as string (non-standard but used by some servers)
	var strError string
	if err := json.Unmarshal(data, &strError); err == nil {
		e.Code = 0 // Unknown code
		e.Message = strError
		return nil
	}

	// Fallback: use raw JSON as message
	e.Code = 0
	e.Message = string(data)
	return nil
}

// Error implements the error interface
func (e *RPCError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("MCP error (%d): %s", e.Code, e.Message)
	}
	return fmt.Sprintf("MCP error: %s", e.Message)
}

// JSONRPCID represents a JSON-RPC ID that can be either a string or a number.
// Per JSON-RPC 2.0 spec, id can be String, Number, or Null.
// Some servers (e.g., Adverity) return string IDs like "1" instead of numeric 1.
type JSONRPCID struct {
	StringValue string
	IntValue    int
	IsString    bool
}

// UnmarshalJSON implements custom unmarshaling to handle both string and number IDs.
func (id *JSONRPCID) UnmarshalJSON(data []byte) error {
	// Try to unmarshal as number first (most common)
	var numID int
	if err := json.Unmarshal(data, &numID); err == nil {
		id.IntValue = numID
		id.StringValue = fmt.Sprintf("%d", numID)
		id.IsString = false
		return nil
	}

	// Try to unmarshal as string
	var strID string
	if err := json.Unmarshal(data, &strID); err == nil {
		id.StringValue = strID
		id.IsString = true
		// Try to parse as int for comparison purposes
		if _, err := fmt.Sscanf(strID, "%d", &id.IntValue); err != nil {
			id.IntValue = 0 // Non-numeric string ID
		}
		return nil
	}

	// Fallback: store raw as string
	id.StringValue = string(data)
	id.IsString = true
	return nil
}

// String returns the string representation of the ID for comparison/logging.
func (id JSONRPCID) String() string {
	return id.StringValue
}

// matchesInt reports whether the ID equals the given integer request id,
// tolerating servers that echo numeric ids back as strings. Non-numeric
// string ids parse to IntValue 0 and can never match (request ids start
// at 1).
func (id JSONRPCID) matchesInt(want int) bool {
	return id.IntValue == want
}

// MarshalJSON implements JSON marshaling.
func (id JSONRPCID) MarshalJSON() ([]byte, error) {
	if id.IsString {
		return json.Marshal(id.StringValue)
	}
	return json.Marshal(id.IntValue)
}

// NewClient creates a new MCP client
func NewClient() *Client {
	return &Client{
		servers: make(map[string]*Server),
	}
}

// AddStdioServer adds a stdio-based MCP server. dir is the working directory the
// subprocess launches in (the client-config bundle root, so relative `mcp/*.py`
// args resolve there); "" inherits the caller's cwd.
func (c *Client) AddStdioServer(ctx context.Context, name, command string, args []string, env map[string]string, dir string) error {
	transport, err := NewStdioTransportInDir(command, args, env, dir)
	if err != nil {
		return fmt.Errorf("failed to create stdio transport: %w", err)
	}

	server := &Server{
		name:         name,
		transport:    transport,
		stdioCommand: command,
		stdioArgs:    args,
		stdioEnv:     env,
		stdioDir:     dir,
	}

	// Initialize the server and get tools
	if err := server.initialize(ctx); err != nil {
		_ = transport.Close()
		return fmt.Errorf("failed to initialize server: %w", err)
	}

	c.mu.Lock()
	c.servers[name] = server
	c.mu.Unlock()

	return nil
}

// AddHTTPServer adds an HTTP-based MCP server
func (c *Client) AddHTTPServer(ctx context.Context, name, url string) error {
	return c.AddHTTPServerWithHeaders(ctx, name, url, nil)
}

// AddHTTPServerWithHeaders adds an HTTP-based MCP server with custom headers for authentication
func (c *Client) AddHTTPServerWithHeaders(ctx context.Context, name, url string, headers map[string]string) error {
	transport := NewHTTPTransportWithHeaders(url, headers)

	server := &Server{
		name:      name,
		transport: transport,
	}

	// Initialize the server and get tools
	if err := server.initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize server: %w", err)
	}

	c.mu.Lock()
	c.servers[name] = server
	c.mu.Unlock()

	return nil
}

// ServerTool pairs an MCP tool with the name of the server that provides it.
type ServerTool struct {
	ServerName string
	Tool       Tool
}

// HasServer reports whether a server is registered with the given name.
// Used by the multi-tenant loader to make repeat AddStdioServer calls
// idempotent — re-loading the same client variant in a single session
// must not spawn a second subprocess.
func (c *Client) HasServer(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.servers[name]
	return ok
}

// GetAllTools returns all tools from all connected servers, preserving server names.
func (c *Client) GetAllTools() []ServerTool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var allTools []ServerTool
	for _, server := range c.servers {
		for _, tool := range server.tools {
			allTools = append(allTools, ServerTool{
				ServerName: server.name,
				Tool:       tool,
			})
		}
	}
	return allTools
}

// CallTool calls a tool by bare name, searching servers in sorted-name
// order so a name collision resolves deterministically. Prefer
// CallToolOn when the caller knows which server it wants — several
// servers export overlapping names (sendgrid and mailbux both have
// send_email), and "first server that has it" is the wrong answer for
// those.
func (c *Client) CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (*ToolResult, error) {
	c.mu.RLock()
	names := make([]string, 0, len(c.servers))
	for name := range c.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var target *Server
	for _, name := range names {
		server := c.servers[name]
		for _, tool := range server.tools {
			if tool.Name == toolName {
				target = server
				break
			}
		}
		if target != nil {
			break
		}
	}
	c.mu.RUnlock()

	if target == nil {
		return nil, fmt.Errorf("tool not found: %s", toolName)
	}
	return target.callTool(ctx, toolName, arguments)
}

// CallToolOn calls toolName on the named server. This is the
// collision-proof routing path: the agent layer registers tools as
// mcp_<server>_<tool> precisely because bare names overlap across
// servers, so the dispatch must carry the server name too.
func (c *Client) CallToolOn(ctx context.Context, serverName, toolName string, arguments map[string]interface{}) (*ToolResult, error) {
	c.mu.RLock()
	server, ok := c.servers[serverName]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("MCP server not found: %s", serverName)
	}
	return server.callTool(ctx, toolName, arguments)
}

// CallToolPrefixed routes an mcp_<server>_<tool> name to the matching
// connected server. Server names may themselves contain underscores, so
// the split is resolved against the live server list: among servers
// whose name is a prefix of the trimmed string, prefer one that
// actually advertises the remainder as a tool, then the longest name.
func (c *Client) CallToolPrefixed(ctx context.Context, fullName string, arguments map[string]interface{}) (*ToolResult, error) {
	const prefix = "mcp_"
	if !strings.HasPrefix(fullName, prefix) {
		return nil, fmt.Errorf("not an MCP tool name: %s", fullName)
	}
	trimmed := strings.TrimPrefix(fullName, prefix)

	c.mu.RLock()
	var (
		best     *Server
		bestTool string
		bestHas  bool
	)
	for name, server := range c.servers {
		rest, ok := strings.CutPrefix(trimmed, name+"_")
		if !ok || rest == "" {
			continue
		}
		has := false
		for _, tool := range server.tools {
			if tool.Name == rest {
				has = true
				break
			}
		}
		better := best == nil ||
			(has && !bestHas) ||
			(has == bestHas && len(name) > len(best.name))
		if better {
			best, bestTool, bestHas = server, rest, has
		}
	}
	c.mu.RUnlock()

	if best == nil {
		return nil, fmt.Errorf("no connected MCP server matches %s", fullName)
	}
	return best.callTool(ctx, bestTool, arguments)
}

// Close closes all server connections
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	for _, server := range c.servers {
		if err := server.transport.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing servers: %v", errs)
	}
	return nil
}

func (s *Server) initialize(ctx context.Context) error {
	// Call initialize method
	result, err := s.transport.Call(ctx, "initialize", map[string]interface{}{
		jsonRPCFieldProtocolVersion: mcpProtocolVersion,
		"capabilities":              map[string]interface{}{},
		"clientInfo": map[string]string{
			jsonRPCFieldName: "fleet",
			"version":        "1.0.0",
		},
	})
	if err != nil {
		return fmt.Errorf("initialize call failed: %w", err)
	}

	// Parse capabilities (we don't strictly need them for now)
	_ = result

	// List available tools
	toolsResult, err := s.transport.Call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return fmt.Errorf("tools/list call failed: %w", err)
	}

	var toolsResponse struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(toolsResult, &toolsResponse); err != nil {
		return fmt.Errorf("failed to parse tools response: %w", err)
	}

	s.tools = toolsResponse.Tools
	return nil
}

func (s *Server) callTool(ctx context.Context, name string, arguments map[string]interface{}) (*ToolResult, error) {
	// Hold the server mutex for the entire call+restart sequence to prevent
	// concurrent callers from using a half-restarted transport.
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.transport.Call(ctx, "tools/call", map[string]interface{}{
		jsonRPCFieldName: name,
		"arguments":      arguments,
	})
	if err != nil {
		// If this is a stdio server and the error looks like a broken pipe / EOF,
		// try to restart the server process and retry the call once.
		if s.stdioCommand != "" && isTransportDeadError(err) {
			log.Printf("MCP server %s appears dead (%v), attempting restart...", s.name, err)
			if restartErr := s.restartLocked(ctx); restartErr != nil {
				return nil, fmt.Errorf("tool call failed and server restart also failed: original=%w, restart=%w", err, restartErr)
			}
			// Only replay the call when the request provably never reached
			// the server (write failure, or a transport poisoned by an
			// earlier cancelled call). A read-side death (EOF after the
			// request was written) means the server may have already
			// executed the call — blindly re-sending a non-idempotent tool
			// like send_email or a deal-create would double-execute it.
			if !isRequestNotDeliveredError(err) {
				return nil, fmt.Errorf(
					"MCP server %s died while executing %s and was restarted; the call's outcome is UNKNOWN — "+
						"verify whether the action took effect (e.g. query for the created object or sent email) "+
						"before re-issuing it: %w", s.name, name, err)
			}
			log.Printf("MCP server %s restarted successfully, retrying tool call (request was never delivered)", s.name)
			result, err = s.transport.Call(ctx, "tools/call", map[string]interface{}{
				jsonRPCFieldName: name,
				"arguments":      arguments,
			})
			if err != nil {
				return nil, fmt.Errorf("tool call failed after server restart: %w", err)
			}
		} else {
			return nil, err
		}
	}

	var toolResult ToolResult
	if err := json.Unmarshal(result, &toolResult); err != nil {
		return nil, fmt.Errorf("failed to parse tool result: %w", err)
	}

	return &toolResult, nil
}

// restartLocked closes the current transport and creates a new one.
// Caller must hold s.mu.
func (s *Server) restartLocked(ctx context.Context) error {
	if s.stdioCommand == "" {
		return fmt.Errorf("restart not supported for non-stdio servers")
	}

	// Close the old transport (ignore errors — it's already broken).
	_ = s.transport.Close()

	transport, err := NewStdioTransportInDir(s.stdioCommand, s.stdioArgs, s.stdioEnv, s.stdioDir)
	if err != nil {
		return fmt.Errorf("failed to create new transport: %w", err)
	}
	s.transport = transport

	if err := s.initialize(ctx); err != nil {
		_ = transport.Close()
		return fmt.Errorf("failed to reinitialize server: %w", err)
	}

	log.Printf("MCP server %s restarted and reinitialized (%d tools)", s.name, len(s.tools))
	return nil
}

// errStdioTransportDead is the sentinel message for a transport poisoned by
// a cancelled call. isTransportDeadError matches on it so the restart path
// in Server.callTool kicks in, and isRequestNotDeliveredError matches on it
// because a transport poisoned before any write means the request for the
// NEXT call never reached the server (safe to replay after restart).
const errStdioTransportDead = "stdio transport marked dead after cancelled call"

// errStdioWriteFailed marks a request that failed before reaching the
// server. isRequestNotDeliveredError matches on it to decide whether a
// restarted call may be retried without double-execution risk.
const errStdioWriteFailed = "stdio write failed (request not delivered)"

// errTransportDesynced marks a stdio transport whose previous call was
// cancelled mid-read: the subprocess may still write the old response,
// so the request/response stream can no longer be trusted. The next
// caller sees this error, which routes through the normal dead-transport
// restart path (fresh subprocess, clean stream).
//
// Its message is errStdioTransportDead, so the substring-matching
// isTransportDeadError and isRequestNotDeliveredError both recognize it
// even when it has been wrapped and the sentinel identity is lost, while
// callers that still hold the unwrapped error can match it precisely with
// errors.Is.
var errTransportDesynced = errors.New(errStdioTransportDead)

// isRequestNotDeliveredError reports whether a failed call's request
// provably never reached the server, making a post-restart retry safe from
// double execution. Two shapes qualify: a write-side failure (broken pipe
// before the request bytes were delivered) and a transport pre-marked dead
// by an earlier cancelled call (the new request was rejected before any
// write).
func isRequestNotDeliveredError(err error) bool {
	if err == nil {
		return false
	}
	// Application-level JSON-RPC errors are never a delivery failure.
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return false
	}
	if errors.Is(err, errTransportDesynced) {
		return true
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "request not delivered") ||
		strings.Contains(errStr, "transport marked dead")
}

// isTransportDeadError returns true if the error indicates the transport
// connection is broken (pipe, EOF, process exit, desync after cancel).
// Application-level JSON-RPC errors never count: a tool error whose
// message happens to contain "EOF" (e.g. a Python parse traceback) must
// not kill and restart a healthy subprocess.
func isTransportDeadError(err error) bool {
	if err == nil {
		return false
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return false
	}
	if errors.Is(err, errTransportDesynced) {
		return true
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "write |1:") ||
		strings.Contains(errStr, "eof") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "process already finished") ||
		strings.Contains(errStr, "file already closed") ||
		strings.Contains(errStr, "transport marked dead")
}

// StdioTransport implements Transport for stdio-based MCP servers
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	reader *bufio.Reader
	mu     sync.Mutex
	nextID int

	// broken is set when a call is cancelled while a response may still
	// be in flight. Once true, the stream framing can't be trusted (the
	// stale response would be read as the answer to the next request),
	// so every subsequent Call fails fast with errTransportDesynced and
	// the server layer restarts the subprocess.
	broken bool
}

// NewStdioTransport spawns an MCP server subprocess in the caller's current
// working directory. Most callers want NewStdioTransportInDir to pin the cwd to
// the client-config bundle root so relative `mcp/*.py` args resolve correctly.
func NewStdioTransport(command string, args []string, env map[string]string) (*StdioTransport, error) {
	return NewStdioTransportInDir(command, args, env, "")
}

// NewStdioTransportInDir is NewStdioTransport with an explicit working directory.
// When dir != "" the subprocess is launched with cmd.Dir = dir, so relative
// command args (e.g. a bundle's `mcp/foo.py`) resolve against the bundle root
// rather than the fleet process cwd (which under systemd is /opt/fleet, NOT the
// /opt/fleet/client bundle checkout — see internal/clientconfig).
func NewStdioTransportInDir(command string, args []string, env map[string]string, dir string) (*StdioTransport, error) {
	cmd := exec.Command(command, args...) //nolint:noctx,gosec // MCP server command comes from trusted config and is intentionally long-running
	cmd.Dir = dir // empty => inherit the caller's cwd (exec.Command's default)

	// Set environment variables with extended PATH for uvx, npx, etc.
	homedir, err := os.UserHomeDir()
	if err != nil {
		// Fallback if UserHomeDir fails, though unlikely
		homedir = os.Getenv("HOME")
	}
	if homedir == "" {
		homedir = "/root" // Sensible default for container if all else fails
	}

	pathEnv := homedir + "/.local/bin:/workspace/go/bin:/usr/local/bin:/usr/bin:/bin"
	cmd.Env = append(cmd.Env, "PATH="+pathEnv)
	cmd.Env = append(cmd.Env, "HOME="+homedir)
	// Suppress Python warnings that pollute stdout
	cmd.Env = append(cmd.Env, "PYTHONWARNINGS=ignore")
	// Ensure Python subprocesses use UTF-8 encoding for STDIO transport.
	// Without these, Python defaults to ASCII in minimal environments,
	// causing UnicodeEncodeError when API responses contain non-ASCII
	// characters (e.g. \xa0 non-breaking spaces from some upstream APIs).
	cmd.Env = append(cmd.Env, "LANG=C.UTF-8")
	cmd.Env = append(cmd.Env, "LC_ALL=C.UTF-8")
	cmd.Env = append(cmd.Env, "PYTHONIOENCODING=utf-8")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	// Forward the subprocess's stderr to ours so import errors / Python
	// tracebacks land in journalctl. Previously this was swallowed, which
	// turned every "missing dep" failure into an opaque "initialize call
	// failed: EOF" message that was nearly impossible to debug from prod.
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		reader: bufio.NewReader(stdout),
	}, nil
}

func (t *StdioTransport) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.broken {
		return nil, errTransportDesynced
	}

	t.nextID++
	id := t.nextID

	request := map[string]interface{}{
		jsonRPCFieldJSONRPC: jsonRPCVersion,
		"id":                id,
		"method":            method,
		"params":            params,
	}

	requestBytes, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	// Write request. Wrap failures with a distinct marker: a write error
	// means the request never reached the server, so Server.callTool may
	// safely retry it after a restart — unlike a read-side failure, where
	// the server may have executed the call before dying.
	if _, err := t.stdin.Write(append(requestBytes, '\n')); err != nil {
		return nil, fmt.Errorf("%s: %w", errStdioWriteFailed, err)
	}

	type result struct {
		line []byte
		err  error
	}

	// Read lines until the response whose id matches this request.
	// Servers may interleave notifications (no id) or log junk between
	// responses; matching on id is what keeps one request from consuming
	// another's answer. One read goroutine is in flight at a time, and a
	// new one is only spawned after the previous delivered into its
	// buffered channel — so cancelling mid-read leaves at most one
	// orphaned reader, and the broken flag guarantees no future Call
	// races it on t.reader before the transport is restarted.
	for {
		resultChan := make(chan result, 1)
		go func() {
			line, err := t.reader.ReadBytes('\n')
			resultChan <- result{line, err}
		}()

		var line []byte
		select {
		case <-ctx.Done():
			// The subprocess may still write the response for this
			// request; the orphaned goroutine above may consume an
			// arbitrary later line. Poison the transport so the next
			// call restarts the subprocess instead of reading garbage.
			t.broken = true
			return nil, ctx.Err()
		case res := <-resultChan:
			if res.err != nil {
				return nil, res.err
			}
			line = res.line
		}

		var response stdioResponse
		if err := json.Unmarshal(line, &response); err != nil {
			// Not JSON-RPC (e.g. a stray library print on stdout) — skip
			// it and keep reading rather than misattributing it to this
			// call or aborting the whole request.
			continue
		}

		// Server-initiated notification/request (carries a method, no
		// matching result/error for us): skip.
		if response.Method != "" && len(response.Result) == 0 && len(response.Error) == 0 {
			continue
		}

		// Server-initiated notification (no id, no result/error): skip.
		if len(response.Result) == 0 && len(response.Error) == 0 {
			continue
		}

		// A response for a different request id — stale leftover or a
		// misbehaving server. Never hand it to this caller.
		if !response.ID.matchesInt(id) {
			log.Printf("MCP stdio: discarding response with id %s while waiting for %d", response.ID.String(), id)
			continue
		}

		if len(response.Error) > 0 && string(response.Error) != nullString {
			rpcErr := &RPCError{}
			if err := rpcErr.UnmarshalJSON(response.Error); err != nil {
				return nil, fmt.Errorf("MCP error (unparseable): %s", string(response.Error))
			}
			return nil, rpcErr
		}

		return response.Result, nil
	}
}

// stdioResponse is the JSON-RPC envelope read off a stdio MCP server's
// stdout. Method is populated on server-initiated notifications/requests so
// Call can skip them while waiting for its own response.
type stdioResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      JSONRPCID       `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// stdioCloseGrace is how long Close waits for a server to exit on stdin
// EOF before killing it. Close is called with the per-server mutex held
// (restart path) and from shutdown, so it must be bounded: a child hung
// mid-tool-call would otherwise wedge every future call on that server
// and stall process shutdown into systemd's SIGKILL.
var stdioCloseGrace = 3 * time.Second

func (t *StdioTransport) Close() error {
	if t.stdin != nil {
		_ = t.stdin.Close()
	}
	if t.cmd.Process == nil {
		return nil
	}

	// Give the child a grace period to exit on stdin EOF (the polite MCP
	// shutdown), then kill. Wait must be running before the kill so the
	// process is always reaped.
	done := make(chan error, 1)
	go func() { done <- t.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(stdioCloseGrace):
		_ = t.cmd.Process.Kill()
		return <-done
	}
}

// HTTPTransport implements Transport for HTTP-based MCP servers
type HTTPTransport struct {
	url       string
	headers   map[string]string
	client    *http.Client
	nextID    int
	mu        sync.Mutex
	sessionID string // MCP session ID captured from initialize response
}

func NewHTTPTransport(url string) *HTTPTransport {
	return NewHTTPTransportWithHeaders(url, nil)
}

func NewHTTPTransportWithHeaders(url string, headers map[string]string) *HTTPTransport {
	return &HTTPTransport{
		url:     url,
		headers: headers,
		client: &http.Client{
			Timeout: DefaultMCPHTTPTimeout,
		},
	}
}

func (t *HTTPTransport) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	sessionID := t.sessionID // Capture under lock
	t.mu.Unlock()

	request := map[string]interface{}{
		jsonRPCFieldJSONRPC: jsonRPCVersion,
		"id":                id,
		"method":            method,
		"params":            params,
	}

	requestBytes, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(requestBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// MCP servers (especially Adverity) require Accept header with both JSON and SSE
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

	// Include MCP session ID if we have one (required after initialize)
	if sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}

	// Apply custom headers (e.g., for authentication)
	// Custom headers can override defaults if needed
	for key, value := range t.headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Capture MCP session ID from response (try multiple header name variants)
	t.captureSessionID(resp.Header)

	// Handle SSE (Server-Sent Events) responses
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return t.parseSSEResponse(resp.Body)
	}

	// Handle standard JSON responses
	return t.parseJSONResponse(resp.Body)
}

// captureSessionID extracts MCP session ID from response headers
// Tries multiple header name variants as servers may use different casing
func (t *HTTPTransport) captureSessionID(headers http.Header) {
	// Try common session ID header names
	sessionHeaders := []string{
		"Mcp-Session-Id",
		"MCP-Session-Id",
		"mcp-session-id",
		"X-Mcp-Session-Id",
	}

	for _, headerName := range sessionHeaders {
		if value := headers.Get(headerName); value != "" {
			t.mu.Lock()
			t.sessionID = value
			t.mu.Unlock()
			return
		}
	}

	// Also try case-insensitive search
	for key, values := range headers {
		if strings.EqualFold(key, "mcp-session-id") && len(values) > 0 {
			t.mu.Lock()
			t.sessionID = values[0]
			t.mu.Unlock()
			return
		}
	}
}

// parseJSONResponse parses a standard JSON-RPC response
func (t *HTTPTransport) parseJSONResponse(body io.Reader) (json.RawMessage, error) {
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      JSONRPCID       `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}

	if err := json.NewDecoder(body).Decode(&response); err != nil {
		return nil, err
	}

	if len(response.Error) > 0 && string(response.Error) != nullString {
		rpcErr := &RPCError{}
		if err := rpcErr.UnmarshalJSON(response.Error); err != nil {
			return nil, fmt.Errorf("MCP error (unparseable): %s", string(response.Error))
		}
		return nil, rpcErr
	}

	return response.Result, nil
}

// parseSSEResponse parses a Server-Sent Events (SSE) stream and extracts JSON-RPC response
// SSE format: lines like "event: message" and "data: {json...}"
// We read data: lines until we find a complete JSON-RPC response
func (t *HTTPTransport) parseSSEResponse(body io.Reader) (json.RawMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024) // allow up to 10MB per SSE line
	var dataBuffer strings.Builder
	var foundData bool

	for scanner.Scan() {
		line := scanner.Text()

		// Skip event: lines and empty lines that don't end an event
		if strings.HasPrefix(line, "event:") {
			continue
		}

		// Handle data: lines
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			if data != "" {
				dataBuffer.WriteString(data)
				foundData = true
			}
			continue
		}

		// Empty line signals end of event - try to parse accumulated data
		if line == "" && foundData {
			jsonData := dataBuffer.String()
			if jsonData != "" {
				result, err := t.tryParseJSONRPCFromSSE(jsonData)
				if err == nil {
					return result, nil
				}
				// Check if this is an RPC error (valid response with error field)
				// vs a parse failure. RPC errors should be returned immediately.
				var rpcErr *RPCError
				if errors.As(err, &rpcErr) {
					return nil, rpcErr
				}
				// If parsing failed (not a JSON-RPC response), continue reading more events
			}
			// Reset for next event
			dataBuffer.Reset()
			foundData = false
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE read error: %w", err)
	}

	// Try parsing any remaining data
	if foundData && dataBuffer.Len() > 0 {
		return t.tryParseJSONRPCFromSSE(dataBuffer.String())
	}

	return nil, fmt.Errorf("no valid JSON-RPC response found in SSE stream")
}

// tryParseJSONRPCFromSSE attempts to parse JSON-RPC response from SSE data
func (t *HTTPTransport) tryParseJSONRPCFromSSE(jsonData string) (json.RawMessage, error) {
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      JSONRPCID       `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}

	if err := json.Unmarshal([]byte(jsonData), &response); err != nil {
		return nil, err
	}

	// Verify this is a JSON-RPC response (has result or error)
	if len(response.Result) == 0 && len(response.Error) == 0 {
		return nil, fmt.Errorf("not a JSON-RPC response")
	}

	if len(response.Error) > 0 && string(response.Error) != nullString {
		rpcErr := &RPCError{}
		if err := rpcErr.UnmarshalJSON(response.Error); err != nil {
			return nil, fmt.Errorf("MCP error (unparseable): %s", string(response.Error))
		}
		return nil, rpcErr
	}

	return response.Result, nil
}

func (t *HTTPTransport) Close() error {
	return nil
}
