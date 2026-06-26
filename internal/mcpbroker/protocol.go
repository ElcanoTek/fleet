// Package mcpbroker carries an MCP tool call across a process boundary.
//
// It is the transport behind the agentcore.MCPBroker seam (issue #167): the
// credentialed MCP client and the connector secrets live in one process (the
// broker), and the agent-loop process holds only a Client that forwards each
// CallMCP over a connection. The symmetry mirrors the in-process path exactly —
//
//	mcpTool -> mcpbroker.Client (agentcore.MCPBroker)
//	            -> [conn] -> mcpbroker.Server -> agentcore.MCPBroker (localMCPBroker)
//	            -> *mcp.Client -> the MCP subprocess
//
// so moving the credential boundary out-of-process does NOT fork a second
// MCP-call path: both ends speak the same agentcore.MCPBroker interface; only a
// connection is interposed.
//
// The package is transport-agnostic — Server.Serve and NewClient take any
// io.ReadWriteCloser. Tests connect the two ends with an in-memory net.Pipe
// (no real process); a later step runs the Server in a child process reached over
// a unix-domain socket. The wire format is line-free JSON values (each frame is a
// self-delimiting JSON object) read with a streaming json.Decoder.
package mcpbroker

// method names the operation a request carries. The envelope is method-based so
// further operations the broker will own (tool/account discovery, per-run
// authorization scoping, credential reload) slot in as new methods WITHOUT a wire
// break — older/newer peers simply reject an unknown method.
type method string

const (
	// methodCall runs an MCP server.tool and returns the rendered result. It is
	// the credential-bearing operation — the whole reason the broker exists.
	methodCall method = "call"
	// methodPing is a liveness probe used to confirm the broker is up and serving
	// before the first real call (and as a cheap health check).
	methodPing method = "ping"
	// methodCancel asks the server to cancel an in-flight request. Its ID field is
	// the ID of the request to cancel (not a new request); it is fire-and-forget
	// (no response is sent for a cancel).
	methodCancel method = "cancel"
	// methodListTools returns the discovered MCP tool catalog (server, tool,
	// description, schema). Once the broker owns the credentialed client, the
	// main process can no longer call GetAllTools locally, so it reads the catalog
	// here. Carries no credentials.
	methodListTools method = "list_tools"
	// methodListAccounts returns the provisioned account names for a server (its
	// Server + BaseVars name the seat). Account names are not secret; the broker
	// resolves them against ITS environment (where the secrets live), so the main
	// process need not hold them.
	methodListAccounts method = "list_accounts"
)

// ToolDescriptor is one entry of the broker's tool catalog — the public shape of
// an MCP tool (no credentials). It mirrors what GetAllTools yields in-process.
type ToolDescriptor struct {
	Server      string         `json:"server"`
	Tool        string         `json:"tool"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// request is a client -> server frame. The server only ever decodes requests; the
// client only ever decodes responses, so the two directions are separate types
// (a streaming json.Decoder reads into one known shape per direction).
type request struct {
	// ID correlates a response to its request, unique per client connection. For a
	// methodCancel frame it is the ID of the in-flight request to cancel.
	ID uint64 `json:"id"`
	// Method selects the operation (methodCall / methodPing / methodCancel).
	Method method `json:"method"`
	// Server / Tool / Args carry the methodCall payload. Credentials are NEVER on
	// the wire — they live only in the broker process and are applied there.
	// Server doubles as the target for methodListAccounts.
	Server string         `json:"server,omitempty"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	// BaseVars names the env-var bases of a server's credential seat, for
	// methodListAccounts (the broker scans ITS env for <BASE>_<ACCOUNT> variants).
	BaseVars []string `json:"baseVars,omitempty"`
}

// response is a server -> client frame answering a request by ID.
type response struct {
	// ID matches the request this answers.
	ID uint64 `json:"id"`
	// Text is the rendered tool output (the broker side already flattened content
	// blocks + applied the fast.io guard/trim, so the wire carries text, not raw
	// blocks).
	Text string `json:"text,omitempty"`
	// IsError is the MCP tool-level error bit (a successful call that reported a
	// tool error). It is DISTINCT from Err: a tool-level error is not a transport
	// failure, and the client surfaces the two differently — exactly as the
	// in-process path distinguishes them.
	IsError bool `json:"isError,omitempty"`
	// Err is a transport/dispatch-level error string (empty on success). It maps
	// back to a non-nil Go error on the client; an unknown method or a server-side
	// failure to run the call lands here.
	Err string `json:"err,omitempty"`
	// Tools answers methodListTools; Accounts answers methodListAccounts. Both are
	// public catalog data (no credentials).
	Tools    []ToolDescriptor `json:"tools,omitempty"`
	Accounts []string         `json:"accounts,omitempty"`
}
