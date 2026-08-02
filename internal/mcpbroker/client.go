package mcpbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// errClientClosed is returned for calls made on, or outstanding when, a Client
// whose connection has closed (peer hangup, transport error, or Close).
var errClientClosed = errors.New("mcpbroker: client connection closed")

var errScopeClosed = errors.New("mcpbroker: scope closed")

const abandonedScopeCloseTimeout = 3 * time.Second

// Client forwards agentcore.MCPBroker calls to a Server over a connection. It is a
// drop-in MCPBroker for the agent loop: the loop calls CallMCP exactly as it would
// the in-process localMCPBroker, but the credentialed work runs in the Server's
// process. Requests multiplex over the one connection, correlated by ID; a single
// background goroutine reads responses and hands each to the waiting caller.
//
// A Client is safe for concurrent use.
type Client struct {
	enc  *json.Encoder
	conn io.Closer
	done chan struct{} // closed when the connection dies or Close is called

	writeMu sync.Mutex    // serializes frame writes (json.Encoder is not concurrency-safe)
	nextID  atomic.Uint64 // per-connection request IDs

	mu       sync.Mutex
	pending  map[uint64]chan response
	closed   bool
	closeErr error
}

var _ agentcore.MCPBroker = (*Client)(nil)

// NewClient starts a Client that reads responses from conn in the background. The
// caller owns conn's lifetime, but Close() (or a transport error) tears the reader
// down and fails any outstanding/subsequent calls.
func NewClient(conn io.ReadWriteCloser) *Client {
	c := &Client{
		enc:     json.NewEncoder(conn),
		conn:    conn,
		done:    make(chan struct{}),
		pending: make(map[uint64]chan response),
	}
	go c.readLoop(conn)
	return c
}

// CallMCP runs server.tool on the broker over the wire, returning the rendered
// text, the tool-level isError bit, and a transport error — the same triple the
// in-process localMCPBroker returns, so the Client is interchangeable with it. The
// tool-level isError (resp.Err == "") stays distinct from a transport error.
func (c *Client) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	return c.callMCP(ctx, "", server, tool, args)
}

func (c *Client) callMCP(ctx context.Context, scope, server, tool string, args map[string]any) (string, bool, error) {
	resp, err := c.roundtrip(ctx, request{
		ID:     c.nextID.Add(1),
		Method: methodCall,
		Scope:  scope,
		Server: server,
		Tool:   tool,
		Args:   args,
	})
	if err != nil {
		return "", false, err
	}
	if resp.Err != "" {
		return resp.Text, resp.IsError, errors.New(resp.Err)
	}
	return resp.Text, resp.IsError, nil
}

// Scope is an isolated per-run MCP client owned by the broker process. Its ID is
// opaque to the caller; Scope implements the same agentcore.MCPBroker seam as
// Client while attaching that ID to every call.
//
// A Scope is safe for concurrent use. Close waits for its active calls, prevents
// new calls, and may be retried if the close request fails.
type Scope struct {
	client  *Client
	id      string
	tools   []ToolDescriptor
	skipped []string

	mu        sync.Mutex
	active    int
	closing   bool
	closed    bool
	drained   chan struct{}
	closeGate chan struct{}
}

var _ agentcore.MCPBroker = (*Scope)(nil)

// OpenScope asks the broker to construct a per-run client from spec. Spec carries
// public account/server names, task identity, or a remote user's email only;
// connector credential values never cross this connection.
func (c *Client) OpenScope(ctx context.Context, spec ScopeSpec) (*Scope, error) {
	resp, err := c.roundtrip(ctx, request{
		ID:        c.nextID.Add(1),
		Method:    methodOpenScope,
		ScopeSpec: spec,
	})
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	if resp.Scope == "" {
		return nil, errors.New("mcpbroker: scope_open returned an empty scope ID")
	}
	return &Scope{
		client:    c,
		id:        resp.Scope,
		tools:     cloneToolDescriptors(resp.Tools),
		skipped:   append([]string(nil), resp.Skipped...),
		closeGate: make(chan struct{}, 1),
	}, nil
}

// CallMCP runs server.tool within this scope.
func (s *Scope) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	s.mu.Lock()
	if s.closed || s.closing {
		s.mu.Unlock()
		return "", false, errScopeClosed
	}
	s.active++
	s.mu.Unlock()
	defer s.finishCall()
	return s.client.callMCP(ctx, s.id, server, tool, args)
}

func (s *Scope) finishCall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	if s.active == 0 && s.closing && s.drained != nil {
		close(s.drained)
		s.drained = nil
	}
}

// Tools returns the public tool catalog discovered for this scope.
func (s *Scope) Tools() []ToolDescriptor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneToolDescriptors(s.tools)
}

// Skipped returns public remote-server names that were selected but unavailable
// while the scope opened. It is empty for bundle scopes.
func (s *Scope) Skipped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.skipped...)
}

func cloneToolDescriptors(src []ToolDescriptor) []ToolDescriptor {
	dst := make([]ToolDescriptor, len(src))
	for i, tool := range src {
		dst[i] = tool
		if tool.InputSchema != nil {
			dst[i].InputSchema = cloneJSONObject(tool.InputSchema)
		}
	}
	return dst
}

func cloneJSONObject(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = cloneJSONValue(value)
	}
	return dst
}

func cloneJSONValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneJSONObject(value)
	case []any:
		dst := make([]any, len(value))
		for i := range value {
			dst[i] = cloneJSONValue(value[i])
		}
		return dst
	default:
		return value
	}
}

// Close releases this scope in the broker. A failed close leaves the scope open
// so callers can retry; a successful close is idempotent locally.
func (s *Scope) Close(ctx context.Context) error {
	select {
	case s.closeGate <- struct{}{}:
		defer func() { <-s.closeGate }()
	case <-ctx.Done():
		return ctx.Err()
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	var drained <-chan struct{}
	if s.active > 0 {
		s.drained = make(chan struct{})
		drained = s.drained
	}
	s.mu.Unlock()

	if drained != nil {
		select {
		case <-drained:
		case <-ctx.Done():
			s.reopenAfterCloseFailure()
			return ctx.Err()
		}
	}

	resp, err := s.client.roundtrip(ctx, request{
		ID:     s.client.nextID.Add(1),
		Method: methodCloseScope,
		Scope:  s.id,
	})
	if err != nil {
		s.reopenAfterCloseFailure()
		return err
	}
	if resp.Err != "" {
		s.reopenAfterCloseFailure()
		return errors.New(resp.Err)
	}
	s.mu.Lock()
	s.closed = true
	s.closing = false
	s.mu.Unlock()
	return nil
}

func (s *Scope) reopenAfterCloseFailure() {
	s.mu.Lock()
	s.closing = false
	s.drained = nil
	s.mu.Unlock()
}

// Ping confirms the Server is up and serving. It returns nil on a reply, or the
// transport/context error otherwise.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.roundtrip(ctx, request{ID: c.nextID.Add(1), Method: methodPing})
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	return nil
}

// ListTools returns the broker's discovered tool catalog — the public catalog the
// main process advertises to the loop once the broker (not the main process) owns
// the credentialed client.
func (c *Client) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	resp, err := c.roundtrip(ctx, request{ID: c.nextID.Add(1), Method: methodListTools})
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return resp.Tools, nil
}

// ListAccounts returns the account names provisioned for server (resolved by the
// broker against its environment from the seat's base var names).
func (c *Client) ListAccounts(ctx context.Context, server string, baseVars []string) ([]string, error) {
	resp, err := c.roundtrip(ctx, request{
		ID:       c.nextID.Add(1),
		Method:   methodListAccounts,
		Server:   server,
		BaseVars: baseVars,
	})
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return resp.Accounts, nil
}

// Reload asks the credential-owning process to re-read and apply its connector
// catalog. The request contains no configuration or credential values; the
// result is a public diff plus the refreshed public tool catalog.
func (c *Client) Reload(ctx context.Context) (*ReloadResult, error) {
	resp, err := c.roundtrip(ctx, request{ID: c.nextID.Add(1), Method: methodReload})
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	if resp.Reload == nil {
		return nil, errors.New("mcpbroker: reload returned an empty result")
	}
	result := *resp.Reload
	result.Tools = cloneToolDescriptors(resp.Reload.Tools)
	result.Summary.Added = append([]string(nil), resp.Reload.Summary.Added...)
	result.Summary.Removed = append([]string(nil), resp.Reload.Summary.Removed...)
	result.Summary.Restarted = append([]string(nil), resp.Reload.Summary.Restarted...)
	result.Summary.Unchanged = append([]string(nil), resp.Reload.Summary.Unchanged...)
	result.Accounts = cloneAccounts(resp.Reload.Accounts)
	result.Servers = cloneServerDescriptors(resp.Reload.Servers)
	return &result, nil
}

func cloneServerDescriptors(src []ServerDescriptor) []ServerDescriptor {
	dst := make([]ServerDescriptor, len(src))
	for i, server := range src {
		dst[i] = server
		dst[i].ToolAllowlist = append([]string(nil), server.ToolAllowlist...)
		dst[i].AccountVars = append([]string(nil), server.AccountVars...)
	}
	return dst
}

func cloneAccounts(src map[string][]string) map[string][]string {
	if src == nil {
		return nil
	}
	dst := make(map[string][]string, len(src))
	for server, accounts := range src {
		dst[server] = append([]string(nil), accounts...)
	}
	return dst
}

// Close tears down the reader and fails outstanding calls, then closes the conn.
func (c *Client) Close() error {
	c.fail(errClientClosed)
	return c.conn.Close()
}

// roundtrip registers a pending slot, sends the request, and waits for its reply,
// the caller's ctx, or the connection dying — whichever comes first. On ctx
// cancellation it best-effort tells the server to cancel the in-flight call.
func (c *Client) roundtrip(ctx context.Context, req request) (response, error) {
	ch := make(chan response, 1)

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return response{}, err
	}
	c.pending[req.ID] = ch
	c.mu.Unlock()

	if err := c.send(req); err != nil {
		c.discard(req.ID)
		return response{}, fmt.Errorf("mcpbroker: send: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		// Best-effort: ask the server to stop the in-flight call so it doesn't keep
		// running an MCP request whose result nobody will read.
		_ = c.send(request{ID: req.ID, Method: methodCancel})
		if req.Method == methodOpenScope {
			// Keep this pending slot alive: cancellation can race the backend creating
			// the scope. If a late response owns an ID, release it instead of losing
			// the only handle and leaking its subprocesses until broker shutdown.
			//nolint:gosec // cleanup must outlive the request context that triggered it.
			go c.closeAbandonedScope(ch)
		} else {
			c.discard(req.ID)
		}
		return response{}, ctx.Err()
	case <-c.done:
		return response{}, c.closedErr()
	}
}

func (c *Client) closeAbandonedScope(ch <-chan response) {
	select {
	case resp := <-ch:
		if resp.Scope == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), abandonedScopeCloseTimeout)
		defer cancel()
		_, _ = c.roundtrip(ctx, request{
			ID:     c.nextID.Add(1),
			Method: methodCloseScope,
			Scope:  resp.Scope,
		})
	case <-c.done:
	}
}

func (c *Client) send(req request) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.enc.Encode(req)
}

func (c *Client) discard(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) readLoop(r io.Reader) {
	dec := json.NewDecoder(r)
	for {
		var resp response
		if err := dec.Decode(&resp); err != nil {
			c.fail(err)
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp // ch is buffered(1) and used once — never blocks
		}
	}
}

// fail marks the client closed (idempotent) and broadcasts via done so every
// waiter unblocks. Outstanding pending slots are abandoned — their waiters return
// through the done branch, and the map is GC'd with the Client.
func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if err == nil || errors.Is(err, io.EOF) {
		c.closeErr = errClientClosed
	} else {
		c.closeErr = err
	}
	close(c.done)
}

func (c *Client) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	return errClientClosed
}
