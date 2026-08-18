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
	"syscall"
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

	// reloadMu serializes Reload (#218) so two concurrent reloads can't both
	// build a server for the same name and leak the loser's transport, and can't
	// interleave their map swaps. It is NOT held during a tool call — only across
	// a reload's diff+swap+drain.
	reloadMu sync.Mutex
}

// Server represents a connection to an MCP server
type Server struct {
	mu        sync.Mutex // protects transport during restart
	name      string
	transport Transport
	// toolsMu guards tools separately from mu: initialize reassigns the slice
	// during a mid-call stdio restart (which holds mu for the whole restart,
	// network round-trip included), while catalog readers (GetAllTools and
	// friends) hold only Client.mu. Guarding reads with mu would block every
	// catalog snapshot behind a hung restart; a dedicated RWMutex makes the
	// assignment race-free without coupling readers to restart latency.
	toolsMu sync.RWMutex
	tools   []Tool

	// def is the descriptor this server was built from, retained so a hot
	// reload (#218) can diff the live server against a new manifest and decide
	// unchanged / restart. Empty for the synthetic inline-http-tools server.
	def ServerDef

	// retired is set under mu when a reload (#218) removes or replaces this
	// server, or when Client.Close shuts it down (#1108) — in both cases after
	// any in-flight call drains and before/as its transport is closed. callTool
	// refuses a call to a retired server, so a caller that captured this
	// *Server just before the registry swap (or racing shutdown) can't
	// resurrect a killed stdio subprocess via the dead-transport restart path
	// (which would leak an unreachable, credential-holding process).
	retired bool

	// Restart state for stdio servers (nil for HTTP servers).
	stdioCommand string
	stdioArgs    []string
	stdioEnv     map[string]string
	stdioDir     string // cwd the subprocess launches in (bundle root); "" = inherit
}

// Transport interface for different MCP connection types
type Transport interface {
	Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error)
	// Notify sends a JSON-RPC notification (no id, no response expected) — used
	// for the MCP lifecycle's notifications/initialized.
	Notify(ctx context.Context, method string, params interface{}) error
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
		def:          ServerDef{Name: name, Command: command, Args: args, Env: env, Dir: dir},
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
	return c.AddHTTPServerWithOptions(ctx, name, url, HTTPServerOptions{Headers: headers})
}

// HTTPServerOptions configures an HTTP MCP server registration. It exists for
// the per-user remote-MCP path (#443): those servers are user-supplied, so they
// must dial through an SSRF-safe HTTP client (HTTPClient) and carry a per-user
// bearer (Headers). The bundle's operator-configured HTTP servers keep using
// AddHTTPServerWithHeaders (default client), unchanged.
type HTTPServerOptions struct {
	// Headers are sent with every request (e.g. Authorization: Bearer <token>).
	Headers map[string]string
	// HTTPClient overrides the default client — pass the SSRF-safe client for
	// user-supplied URLs. nil = the default bounded client.
	HTTPClient *http.Client
	// TLS, when non-nil, hardens the handshake (CA pinning / mTLS / public-key
	// pin) for an operator-configured server (#280). Ignored when HTTPClient is
	// set (that caller already owns the full client). nil = default system TLS.
	TLS *TLSOptions
}

// AddHTTPServerWithOptions adds an HTTP-based MCP server with full control over
// the headers and the underlying HTTP client.
func (c *Client) AddHTTPServerWithOptions(ctx context.Context, name, url string, opts HTTPServerOptions) error {
	transport := NewHTTPTransportWithHeaders(url, opts.Headers)
	switch {
	case opts.HTTPClient != nil:
		// The caller fully owns the client (e.g. the SSRF-safe per-user client);
		// TLS options, if any, are its responsibility, not ours.
		transport.client = opts.HTTPClient
	case opts.TLS != nil && !opts.TLS.IsZero():
		// Fail closed: a TLSClientConfig is applied by http.Transport only to
		// https requests, so requesting hardening for a plaintext url would
		// silently connect unverified. Refuse to register rather than mislead.
		if !strings.HasPrefix(strings.ToLower(url), "https://") {
			return fmt.Errorf("mcp server %q: TLS hardening (tls) requires an https url, got %q", name, url)
		}
		tlsCfg, err := opts.TLS.build()
		if err != nil {
			return fmt.Errorf("mcp server %q: tls: %w", name, err)
		}
		if tlsCfg != nil {
			transport.client = tlsHTTPClient(tlsCfg)
		}
	}

	server := &Server{
		name:      name,
		transport: transport,
		// Retain the operator-configured descriptor for hot-reload diffing
		// (#218). The per-user SSRF client (opts.HTTPClient) case is never
		// manifest-reloaded, so leaving its def http-shaped is harmless.
		def: ServerDef{Name: name, URL: url, Headers: opts.Headers, TLS: opts.TLS},
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
		for _, tool := range server.toolsSnapshot() {
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
		for _, tool := range server.toolsSnapshot() {
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
		for _, tool := range server.toolsSnapshot() {
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

// Close closes all server connections. Each server is retired under its own
// mutex — mirroring reload's drainAndClose — because just closing transports
// left a race (#1108): an in-flight callTool whose transport died while Close
// ran matched isTransportDeadError and restartLocked respawned a credentialed
// subprocess AFTER Close returned, with nothing left to ever close it (broker
// mode's process-group SIGKILL contains that; in-process mode leaked it).
// Taking Server.mu waits for the in-flight call — bounded, since every
// transport Call respects its context — then closes whatever transport that
// call left behind (a restarted one included), and retired refuses every
// later call instead of respawning.
func (c *Client) Close() error {
	c.mu.RLock()
	servers := make([]*Server, 0, len(c.servers))
	for _, server := range c.servers {
		servers = append(servers, server)
	}
	c.mu.RUnlock()

	// Retire concurrently, matching Reload's drain: one server with a long
	// in-flight call (or a hung child eating the full stdioCloseGrace) must
	// not delay shutting down the others.
	var (
		wg     sync.WaitGroup
		errsMu sync.Mutex
		errs   []error
	)
	for _, server := range servers {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			s.mu.Lock()
			defer s.mu.Unlock()
			s.retired = true
			if err := s.transport.Close(); err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
		}(server)
	}
	wg.Wait()

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

	// Validate the negotiated protocol version. A mismatch is not fatal (the
	// spec lets a server answer with a version it supports), but surface it so
	// an incompatible server is visible rather than silently discarded.
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(result, &initResult); err == nil &&
		initResult.ProtocolVersion != "" && initResult.ProtocolVersion != mcpProtocolVersion {
		log.Printf("MCP %s: server negotiated protocolVersion %q; client pinned %q",
			s.name, initResult.ProtocolVersion, mcpProtocolVersion)
	}

	// MCP lifecycle: the client MUST send notifications/initialized after the
	// initialize result and before any other request. Lenient (FastMCP) servers
	// tolerate its absence, but a strict/third-party server is within spec to
	// reject tools/list or tools/call until it arrives.
	if err := s.transport.Notify(ctx, "notifications/initialized", map[string]interface{}{}); err != nil {
		return fmt.Errorf("notifications/initialized failed: %w", err)
	}

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

	s.toolsMu.Lock()
	s.tools = toolsResponse.Tools
	s.toolsMu.Unlock()
	return nil
}

// toolsSnapshot returns the server's current tool slice under the tools lock.
// Catalog readers must use this instead of touching s.tools directly — a
// mid-call stdio restart reassigns the slice concurrently (see toolsMu).
func (s *Server) toolsSnapshot() []Tool {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return s.tools
}

func (s *Server) callTool(ctx context.Context, name string, arguments map[string]interface{}) (*ToolResult, error) {
	// Hold the server mutex for the entire call+restart sequence to prevent
	// concurrent callers from using a half-restarted transport.
	s.mu.Lock()
	defer s.mu.Unlock()

	// A reload (#218) or Client.Close (#1108) may have retired this server
	// (closed its transport, removed it from service) between a caller capturing
	// the *Server and reaching here. Refuse rather than call a closed transport —
	// for a stdio server that would otherwise trip the dead-transport restart
	// path and spawn an orphaned, unreachable subprocess. Both retirers set
	// `retired` under this same mutex, so a call already in flight when
	// retirement lands completes first (and the retirer closes whatever
	// transport it leaves behind).
	if s.retired {
		return nil, fmt.Errorf("MCP server %s was retired (removed by a reload or client shutdown)", s.name)
	}

	// A nil arguments map marshals to "arguments": null, which strict MCP
	// servers reject with -32602 (arguments must be an object when present).
	// Empty args arrive as nil after a JSON round-trip that drops empty maps.
	if arguments == nil {
		arguments = map[string]interface{}{}
	}

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

	log.Printf("MCP server %s restarted and reinitialized (%d tools)", s.name, len(s.toolsSnapshot()))
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
//
// Detection is layered (#1108). Sentinel and stdlib identities come first:
// the exec pipe errors this path actually sees keep their wrap chain intact
// (*fs.PathError → syscall.Errno / os.ErrClosed, io.EOF from the reader), so
// errors.Is matches them exactly. A small set of precise string forms backs
// that up for errors whose chain was flattened (fmt.Errorf with %v, an error
// rebuilt from a message). The old bare "eof" substring match is gone — it
// classified ANY message containing those three letters (e.g. "whereof") as
// a dead transport and restarted a healthy subprocess; EOF now only matches
// as a standalone word, the exact way io.EOF renders.
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
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, os.ErrClosed) ||
		errors.Is(err, os.ErrProcessDone) {
		return true
	}
	errStr := err.Error()
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "process already finished") ||
		strings.Contains(lower, "file already closed") ||
		strings.Contains(lower, "transport marked dead") ||
		containsEOFToken(errStr)
}

// containsEOFToken reports whether s contains "EOF" as a standalone word —
// the exact rendering of io.EOF and its stdlib wrappers ("EOF",
// "unexpected EOF"), including when the wrap chain was flattened into a
// plain string. Deliberately case-sensitive and word-bounded so an
// application message that merely contains the letters ("EOFError: bad
// CSV", "whereof") can never be misread as a dead transport.
func containsEOFToken(s string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], "EOF")
		if j < 0 {
			return false
		}
		j += i
		leftOK := j == 0 || !isWordByte(s[j-1])
		rightOK := j+3 == len(s) || !isWordByte(s[j+3])
		if leftOK && rightOK {
			return true
		}
		i = j + 3
	}
}

// isWordByte reports whether c is an ASCII letter, digit, or underscore —
// the byte classes that would glue "EOF" into a larger identifier.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
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
	cmd.Dir = dir                         // empty => inherit the caller's cwd (exec.Command's default)

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

// stdioResponseCaptureCap bounds how many bytes of a single stdout line from a
// stdio MCP server are held in host memory. In broker mode the subprocess runs
// host-side, so one response line — a connector returning a giant query result
// or a fetched page — was previously buffered whole before any downstream
// truncation applied, letting a single data-driven oversized response OOM the
// fleet process. Same 64 MiB ceiling as the sandbox bridge's response cap
// (bridgeResponseCaptureCap, itself pinned to BashOutputCaptureCap); bytes past
// it are drained and counted (see readCappedLine) rather than stored.
const stdioResponseCaptureCap = 64 * 1024 * 1024

// readCappedLine reads one newline-terminated line from r, keeping at most
// limit bytes and draining-but-counting the rest. Duplicated by hand from
// internal/sandbox's readCappedLine (bridge path) — this package imports no
// internal packages and the sandbox is the container runtime, so the 15 lines
// are copied rather than depended on; keep the two in sync. Draining to the
// delimiter keeps the stream framed for the next response even when a line
// overflows the cap. The returned data includes the newline when it fell
// within the cap; err mirrors bufio.Reader.ReadBytes (nil means the delimiter
// was found).
func readCappedLine(r *bufio.Reader, limit int) (data []byte, discarded int64, err error) {
	for {
		frag, ferr := r.ReadSlice('\n')
		keep := limit - len(data)
		if keep > len(frag) {
			keep = len(frag)
		}
		if keep > 0 {
			// ReadSlice's bytes are only valid until the next read — copy.
			data = append(data, frag[:keep]...)
		}
		discarded += int64(len(frag) - keep)
		if errors.Is(ferr, bufio.ErrBufferFull) {
			continue
		}
		return data, discarded, ferr
	}
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

	// Write request. Failures carry a distinct marker: a write error means
	// the request never reached the server, so Server.callTool may safely
	// retry it after a restart — unlike a read-side failure, where the
	// server may have executed the call before dying.
	if err := t.writeLocked(ctx, append(requestBytes, '\n')); err != nil {
		return nil, err
	}

	type result struct {
		line      []byte
		discarded int64
		err       error
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
			line, discarded, err := readCappedLine(t.reader, stdioResponseCaptureCap)
			resultChan <- result{line, discarded, err}
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
			if res.discarded > 0 {
				// The line overran the cap, so its JSON cannot be parsed —
				// fail this call loudly rather than hand anyone a truncated
				// response. The reader drained to the newline, so the stream
				// is still framed and the transport stays usable: no broken
				// flag, and the message must not match isTransportDeadError's
				// markers (the subprocess is healthy — a restart would fix
				// nothing). If the oversized line was an interleaved
				// notification rather than this call's response, the real
				// response is consumed by a later call's id match and
				// discarded there — defined either way.
				return nil, fmt.Errorf("MCP stdio: response line exceeded %d bytes (%d bytes discarded) and was dropped — narrow the query or page the tool's results", stdioResponseCaptureCap, res.discarded)
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

// Notify writes a JSON-RPC notification (no id, no response read) to the
// subprocess. The write honors ctx like Call's does: even a small
// notification blocks forever against a full pipe whose child stopped
// reading stdin.
func (t *StdioTransport) Notify(ctx context.Context, method string, params interface{}) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.broken {
		return errTransportDesynced
	}
	msg := map[string]interface{}{
		jsonRPCFieldJSONRPC: jsonRPCVersion,
		"method":            method,
		"params":            params,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return t.writeLocked(ctx, append(b, '\n'))
}

// writeLocked writes b to the subprocess's stdin, honoring ctx. The write
// runs in a goroutine with a select on ctx.Done() — the same pattern as the
// read side — because a write to a full pipe (64 KiB kernel buffer) against
// a child that stopped reading stdin blocks forever, and model-authored
// tools/call arguments easily exceed 64 KiB. Without this, one wedged
// connector cascaded (#1108): Server.callTool holds Server.mu for the whole
// call, so every future call to that server hung; drainAndClose blocked on
// that mutex; and Reload blocked in its drain while holding reloadMu —
// permanently disabling hot-reload, un-cancellable from the parent.
//
// On timeout/cancel the transport is poisoned (broken): part of the request
// may already sit in the pipe, so the stream framing can't be trusted, and
// the orphaned writer may still complete later. The next Call fails fast
// with errTransportDesynced and the server layer restarts the subprocess —
// whose teardown closes stdin, which also unblocks the orphaned writer.
// Errors carry the errStdioWriteFailed marker: the child never received the
// full request line, so a post-restart replay is double-execution-safe.
// Caller must hold t.mu.
func (t *StdioTransport) writeLocked(ctx context.Context, b []byte) error {
	writeDone := make(chan error, 1)
	go func() {
		_, err := t.stdin.Write(b)
		writeDone <- err
	}()
	select {
	case <-ctx.Done():
		// Both select cases can be ready when cancellation races a
		// fast write; Go picks one at random. A completed write must
		// not be reported as "request not delivered" — the child has
		// the full line and may execute the call, and that error text
		// invites re-issuing a non-idempotent tool. Drain writeDone
		// non-blockingly: a finished write keeps its honest outcome
		// (success falls through to the read side's ctx handling).
		select {
		case err := <-writeDone:
			if err != nil {
				return fmt.Errorf("%s: %w", errStdioWriteFailed, err)
			}
			return nil
		default:
		}
		t.broken = true
		return fmt.Errorf("%s: %w", errStdioWriteFailed, ctx.Err())
	case err := <-writeDone:
		if err != nil {
			return fmt.Errorf("%s: %w", errStdioWriteFailed, err)
		}
		return nil
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
			// headers may carry a resolved bearer/API key; never forward it to
			// a host a 30x names (stdlib only strips Authorization/Cookie).
			CheckRedirect: stripHeadersOnCrossOriginRedirect,
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
		return t.parseSSEResponse(resp.Body, id)
	}

	// Handle standard JSON responses
	return t.parseJSONResponse(resp.Body, id)
}

// Notify POSTs a JSON-RPC notification (no id) and discards the (typically
// empty / 202 Accepted) response. Used for notifications/initialized.
func (t *HTTPTransport) Notify(ctx context.Context, method string, params interface{}) error {
	t.mu.Lock()
	sessionID := t.sessionID
	t.mu.Unlock()

	msg := map[string]interface{}{
		jsonRPCFieldJSONRPC: jsonRPCVersion,
		"method":            method,
		"params":            params,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}
	for key, value := range t.headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	t.captureSessionID(resp.Header)
	// A notification has no response body; drain (bounded — the bytes are
	// discarded, but time inside the client timeout shouldn't be burned on a
	// server streaming garbage past the response cap).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, int64(httpResponseCaptureCap)))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("notification %s: unexpected status %d", method, resp.StatusCode)
	}
	return nil
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

// httpResponseCaptureCap bounds how many bytes of an HTTP or SSE MCP
// response body are held in host memory — the same 64 MiB ceiling as the
// stdio path's stdioResponseCaptureCap (#1108). The 2-minute client timeout
// bounds duration, not bytes: without this cap a hostile or buggy server —
// including a user-supplied remote one (#443), which remotemcp.probeServer
// and the per-run overlay both route through this transport — could stream
// gigabytes into the credential-owning process within the timeout and OOM
// it. A var (not const) only so tests can exercise the bound without
// allocating 64 MiB; production code never reassigns it.
var httpResponseCaptureCap = stdioResponseCaptureCap

// parseJSONResponse parses a standard JSON-RPC response, reading at most
// httpResponseCaptureCap bytes; past the cap the call fails with a clean
// per-call error instead of buffering the body unbounded. wantID is the
// JSON-RPC id of the request this response answers: a mismatch is rejected
// (the stdio path matches ids the same way) rather than misattributed.
func (t *HTTPTransport) parseJSONResponse(body io.Reader, wantID int) (json.RawMessage, error) {
	var response struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      JSONRPCID       `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}

	// N is cap+1 so "hit the cap" is distinguishable from "read exactly cap
	// bytes of valid JSON": a decode failure with the limiter exhausted means
	// the body overran the ceiling, not that its JSON was malformed.
	limited := &io.LimitedReader{R: body, N: int64(httpResponseCaptureCap) + 1}
	if err := json.NewDecoder(limited).Decode(&response); err != nil {
		if limited.N <= 0 {
			return nil, fmt.Errorf("MCP http: response exceeded %d bytes and was dropped — narrow the query or page the tool's results", httpResponseCaptureCap)
		}
		return nil, err
	}

	if !response.ID.matchesInt(wantID) {
		// A server that cannot attribute a request answers with id:null
		// (or a wrong id), and its error member is the only diagnostic
		// it offers. The call still fails — a response that doesn't
		// match the request id must never be attributed as its result —
		// but surface that error text so the operator debugs the
		// server's real complaint, not a phantom id mismatch.
		if len(response.Error) > 0 && string(response.Error) != nullString {
			return nil, fmt.Errorf("MCP http: response id %s does not match request id %d (response carried error: %s)", response.ID.String(), wantID, string(response.Error))
		}
		return nil, fmt.Errorf("MCP http: response id %s does not match request id %d", response.ID.String(), wantID)
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

// parseSSEResponse parses a Server-Sent Events (SSE) stream and extracts the
// JSON-RPC response whose id matches wantID. SSE format: lines like
// "event: message" and "data: {json...}"; data: lines are accumulated until
// a complete JSON-RPC response is found. The whole stream is read through a
// limiter at httpResponseCaptureCap (#1108) — which also bounds dataBuffer's
// total growth, since it only accumulates bytes read from the stream — so an
// endless or oversized stream fails this call cleanly instead of growing
// host memory; a single SSE line is additionally capped at 10MB by the
// scanner. Responses carrying a different id (an interleaved stale event)
// are skipped, mirroring the stdio path's id matching.
func (t *HTTPTransport) parseSSEResponse(body io.Reader, wantID int) (json.RawMessage, error) {
	limited := &io.LimitedReader{R: body, N: int64(httpResponseCaptureCap) + 1}
	scanner := bufio.NewScanner(limited)
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
				result, err := t.tryParseJSONRPCFromSSE(jsonData, wantID)
				if err == nil {
					return result, nil
				}
				// Check if this is an RPC error (valid response with error field)
				// vs a parse failure. RPC errors should be returned immediately.
				var rpcErr *RPCError
				if errors.As(err, &rpcErr) {
					return nil, rpcErr
				}
				// If parsing failed (not a JSON-RPC response, or a response to a
				// different request id), continue reading more events
			}
			// Reset for next event
			dataBuffer.Reset()
			foundData = false
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE read error: %w", err)
	}

	// The limiter ran dry before a matching response completed: the stream
	// overran the cap. Fail loudly rather than hand back a truncated tail
	// (the scanner sees the cut as a plain EOF, so without this check the
	// error would be a misleading "no valid JSON-RPC response").
	if limited.N <= 0 {
		return nil, fmt.Errorf("MCP http: SSE response exceeded %d bytes without a complete JSON-RPC response and was dropped — narrow the query or page the tool's results", httpResponseCaptureCap)
	}

	// Try parsing any remaining data
	if foundData && dataBuffer.Len() > 0 {
		return t.tryParseJSONRPCFromSSE(dataBuffer.String(), wantID)
	}

	return nil, fmt.Errorf("no valid JSON-RPC response found in SSE stream")
}

// tryParseJSONRPCFromSSE attempts to parse a JSON-RPC response to request
// wantID from SSE data. A well-formed response (result or error) whose id
// differs from wantID returns a plain error so the SSE loop skips it and
// keeps reading — the same treatment stdio gives a stale response line —
// rather than attributing another request's result (or error) to this call.
func (t *HTTPTransport) tryParseJSONRPCFromSSE(jsonData string, wantID int) (json.RawMessage, error) {
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

	// Never hand this caller a response for a different request — checked
	// before the error branch so a stale error isn't returned as this call's.
	if !response.ID.matchesInt(wantID) {
		log.Printf("MCP http: discarding SSE response with id %s while waiting for %d", response.ID.String(), wantID)
		return nil, fmt.Errorf("JSON-RPC response id %s does not match request id %d", response.ID.String(), wantID)
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
