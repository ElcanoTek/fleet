package ms365mcp

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

//go:embed tools_catalog.json
var embeddedToolsCatalog []byte

const mcpServerInstructions = `Microsoft 365 Graph MCP server.
- Mail messages from list-mail-messages, list-mail-folder-messages, and get-mail-message automatically include expanded attachment metadata (id, name, contentType, size, isInline).
- To download a mail attachment or drive file to disk, call get-mail-attachment-content or get-download-url to obtain a pre-authenticated downloadUrl, then pass that downloadUrl to download_url.`

// ToolSpec describes one MCP tool exposed by the Microsoft 365 MCP server.
type ToolSpec struct {
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	InputSchema       json.RawMessage `json:"inputSchema"`
	Annotations       json.RawMessage `json:"annotations,omitempty"`
	Critical          bool            `json:"critical,omitempty"`
	PathPattern       string          `json:"pathPattern"`
	Method            string          `json:"method"`
	PathParams        []string        `json:"pathParams"`
	QueryParams       []string        `json:"queryParams"`
	HeaderParams      []string        `json:"headerParams"`
	BodyParams        []string        `json:"bodyParams"`
	ContentType       string          `json:"contentType"`
	SupportsTimezone  bool            `json:"supportsTimezone"`
	ReturnDownloadURL bool            `json:"returnDownloadUrl"`
}

// Handler serves the Microsoft 365 OAuth 2.0 endpoints, the stateless
// Streamable HTTP MCP endpoint (/mcp), and single-use attachment downloads
// (/attachment).
type Handler struct {
	cfg         Config
	pkce        pkceStore
	tickets     *AttachmentTicketStore
	tools       []ToolSpec
	toolsByName map[string]ToolSpec
}

// NewHandler constructs a Handler for the given Config.
func NewHandler(cfg Config) *Handler {
	if cfg.TenantID == "" {
		cfg.TenantID = defaultTenantID
	}
	if cfg.AttachmentURLTTLSeconds <= 0 {
		cfg.AttachmentURLTTLSeconds = defaultAttachmentURLTTLSeconds
	}
	if cfg.GraphBaseURL == "" {
		cfg.GraphBaseURL = defaultGraphBaseURL
	}
	if cfg.AuthBaseURL == "" {
		cfg.AuthBaseURL = defaultAuthBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	var specs []ToolSpec
	if err := json.Unmarshal(embeddedToolsCatalog, &specs); err != nil {
		panic(fmt.Sprintf("ms365mcp: unmarshal embedded tools catalog: %v", err))
	}
	byName := make(map[string]ToolSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}

	return &Handler{
		cfg:         cfg,
		tickets:     NewAttachmentTicketStore(),
		tools:       specs,
		toolsByName: byName,
	}
}

func (h *Handler) httpClient() *http.Client {
	if h.cfg.HTTPClient != nil {
		return h.cfg.HTTPClient
	}
	return http.DefaultClient
}

func (h *Handler) streamHTTPClient() *http.Client {
	if h.cfg.StreamHTTPClient != nil {
		return h.cfg.StreamHTTPClient
	}
	if h.cfg.HTTPClient != nil && h.cfg.HTTPClient.Transport != nil {
		return &http.Client{Transport: h.cfg.HTTPClient.Transport}
	}
	return newDefaultStreamHTTPClient()
}

// Tickets returns the handler's attachment ticket store.
func (h *Handler) Tickets() *AttachmentTicketStore {
	return h.tickets
}

// ToolNames returns the names of all registered MCP tools in catalog order.
func (h *Handler) ToolNames() []string {
	out := make([]string, len(h.tools))
	for i, t := range h.tools {
		out[i] = t.Name
	}
	return out
}

// ServeHTTP dispatches requests across the Microsoft 365 OAuth, MCP, and
// attachment endpoints.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/.well-known/oauth-authorization-server":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleOAuthAuthorizationServer(w, r)
	case path == "/.well-known/oauth-protected-resource" || strings.HasPrefix(path, "/.well-known/oauth-protected-resource/"):
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleOAuthProtectedResource(w, r)
	case path == "/register":
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleDynamicClientRegistration(w, r)
	case path == "/authorize":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleAuthorize(w, r)
	case path == "/token":
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleToken(w, r)
	case path == "/attachment":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleAttachment(w, r)
	case path == "/mcp":
		switch r.Method {
		case http.MethodPost:
			h.handleMCPPost(w, r)
		case http.MethodGet:
			h.handleMCPGet(w, r)
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleMCPGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    -32000,
			"message": "Method not allowed.",
		},
		"id": nil,
	})
}

func (h *Handler) writeMCPUnauthorized(w http.ResponseWriter, r *http.Request, desc string) {
	baseURL := h.cfg.baseURLForRequest(r)
	resourceMetadataURL := baseURL + "/.well-known/oauth-protected-resource"
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer resource_metadata="%s", error="invalid_token", error_description="%s"`,
		resourceMetadataURL,
		strings.ReplaceAll(desc, `"`, `'`),
	))
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error": "Unauthorized",
	})
}

func extractBearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

func isJWTExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		// Non-JWT opaque token: let upstream Graph validate it.
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payloadBytes, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return false
		}
	}
	var claims struct {
		Exp *int64 `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil || claims.Exp == nil {
		return false
	}
	// 60-second clock-skew buffer matching softeria auth.js.
	return time.Now().Unix() >= (*claims.Exp - 60)
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var errGraphUnauthorized = errors.New("microsoft graph returned 401 unauthorized")

func (h *Handler) handleMCPPost(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		h.writeMCPUnauthorized(w, r, "Missing Bearer token")
		return
	}
	if h.cfg.DisableAuthenticatedMCP {
		writeJSON(w, http.StatusForbidden, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &jsonRPCError{Code: -32001, Message: "Microsoft 365 Graph MCP execution is hosted in the credential broker process"},
		})
		return
	}
	if isJWTExpired(token) {
		h.writeMCPUnauthorized(w, r, "Access token expired")
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &jsonRPCError{Code: -32700, Message: "Failed to read request body"},
		})
		return
	}
	trimmed := bytes.TrimSpace(bodyBytes)
	if len(trimmed) == 0 {
		writeJSON(w, http.StatusBadRequest, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &jsonRPCError{Code: -32700, Message: "Empty request body"},
		})
		return
	}

	if trimmed[0] == '[' {
		var batch []jsonRPCRequest
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			writeJSON(w, http.StatusBadRequest, jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &jsonRPCError{Code: -32700, Message: "Parse error"},
			})
			return
		}
		var responses []jsonRPCResponse
		for _, req := range batch {
			resp, isNotif, err := h.dispatchJSONRPC(r, token, req)
			if errors.Is(err, errGraphUnauthorized) {
				h.writeMCPUnauthorized(w, r, "Upstream Microsoft Graph rejected the access token")
				return
			}
			if !isNotif {
				responses = append(responses, resp)
			}
		}
		if len(responses) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, responses)
		return
	}

	var rpcReq jsonRPCRequest
	if err := json.Unmarshal(trimmed, &rpcReq); err != nil {
		writeJSON(w, http.StatusBadRequest, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error:   &jsonRPCError{Code: -32700, Message: "Parse error"},
		})
		return
	}
	resp, isNotif, err := h.dispatchJSONRPC(r, token, rpcReq)
	if errors.Is(err, errGraphUnauthorized) {
		h.writeMCPUnauthorized(w, r, "Upstream Microsoft Graph rejected the access token")
		return
	}
	if isNotif {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) dispatchJSONRPC(r *http.Request, token string, req jsonRPCRequest) (jsonRPCResponse, bool, error) {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null" && strings.HasPrefix(req.Method, "notifications/")
	if len(req.ID) == 0 {
		isNotification = true
	}

	switch req.Method {
	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return jsonRPCResponse{}, true, nil

	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		proto := "2024-11-05"
		switch params.ProtocolVersion {
		case "2024-11-05", "2025-03-26", "2025-06-18":
			proto = params.ProtocolVersion
		}
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Result: map[string]any{
				"protocolVersion": proto,
				"capabilities": map[string]any{
					"tools": map[string]any{
						"listChanged": false,
					},
				},
				"serverInfo": map[string]any{
					"name":    "ms-365-mcp-server",
					"version": "1.0.0",
				},
				"instructions": mcpServerInstructions,
			},
		}, false, nil

	case "ping":
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Result:  map[string]any{},
		}, false, nil

	case "prompts/list":
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Result: map[string]any{
				"prompts": []any{},
			},
		}, false, nil

	case "resources/list":
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Result: map[string]any{
				"resources": []any{},
			},
		}, false, nil

	case "tools/list":
		toolList := make([]map[string]any, 0, len(h.tools))
		for _, t := range h.tools {
			item := map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			}
			if len(t.Annotations) > 0 && string(t.Annotations) != "null" {
				item["annotations"] = t.Annotations
			}
			if t.Critical {
				item["critical"] = true
			}
			toolList = append(toolList, item)
		}
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Result: map[string]any{
				"tools": toolList,
			},
		}, false, nil

	case "tools/call":
		resp, unauth := h.handleToolsCall(r, req, token)
		if unauth {
			return jsonRPCResponse{}, false, errGraphUnauthorized
		}
		return resp, false, nil

	default:
		if isNotification {
			return jsonRPCResponse{}, true, nil
		}
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Error:   &jsonRPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
		}, false, nil
	}
}

func defaultID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func (h *Handler) handleToolsCall(r *http.Request, req jsonRPCRequest, token string) (jsonRPCResponse, bool) {
	var callParams struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &callParams); err != nil {
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Error:   &jsonRPCError{Code: -32602, Message: "Invalid tool call params: " + err.Error()},
		}, false
	}
	if callParams.Arguments == nil {
		callParams.Arguments = map[string]any{}
	}
	res, isErr, err := h.executeToolCall(r.Context(), r, token, callParams.Name, callParams.Arguments)
	if errors.Is(err, errGraphUnauthorized) {
		return jsonRPCResponse{}, true
	}
	if err != nil {
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      defaultID(req.ID),
			Result:  formatToolResult(map[string]any{"error": err.Error()}, true),
		}, false
	}
	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      defaultID(req.ID),
		Result:  formatToolResult(res, isErr),
	}, false
}

func formatToolResult(payload any, isError bool) map[string]any {
	var text string
	switch v := payload.(type) {
	case string:
		text = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			text = fmt.Sprintf(`{"error":%q}`, err.Error())
			isError = true
		} else {
			text = string(b)
		}
	}
	out := map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": text,
			},
		},
	}
	if isError {
		out["isError"] = true
	}
	return out
}
