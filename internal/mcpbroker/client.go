package mcpbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// errClientClosed is returned for calls made on, or outstanding when, a Client
// whose connection has closed (peer hangup, transport error, or Close).
var errClientClosed = errors.New("mcpbroker: client connection closed")

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
	resp, err := c.roundtrip(ctx, request{
		ID:     c.nextID.Add(1),
		Method: methodCall,
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
		c.discard(req.ID)
		// Best-effort: ask the server to stop the in-flight call so it doesn't keep
		// running an MCP request whose result nobody will read.
		_ = c.send(request{ID: req.ID, Method: methodCancel})
		return response{}, ctx.Err()
	case <-c.done:
		return response{}, c.closedErr()
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
