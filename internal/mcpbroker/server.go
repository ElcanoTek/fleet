package mcpbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// Backend is what a Server serves: the credential-bearing MCP call seam plus the
// read-only discovery the main process can no longer do for itself once the broker
// owns the client — the tool catalog and the provisioned account names. The broker
// process implements it over the real credentialed *mcp.Client + creds; tests fake
// it. CallMCP is the SAME agentcore.MCPBroker the in-process loop uses (no second
// governed call path, issue #167); discovery returns only public catalog data.
type Backend interface {
	agentcore.MCPBroker
	// ListTools returns the discovered tool catalog (public shape, no credentials).
	ListTools(ctx context.Context) ([]ToolDescriptor, error)
	// ListAccounts returns the account names provisioned for server, resolved
	// against the broker's environment from the seat's base var names.
	ListAccounts(ctx context.Context, server string, baseVars []string) ([]string, error)
}

// ScopedBackend optionally extends Backend with isolated per-run MCP clients.
// The production credential owner implements this interface when scoped clients
// are available; older backends continue serving unscoped calls and receive a
// clear error if a peer requests a scope.
type ScopedBackend interface {
	// OpenScope returns public discovery metadata only. skipped contains remote
	// server names, never connection errors or credential-bearing details.
	OpenScope(ctx context.Context, spec ScopeSpec) (scopeID string, tools []ToolDescriptor, skipped []string, err error)
	// CallMCPInScope must reject unknown scope IDs. CloseScope must coordinate
	// with calls that reached the backend before it, because cancellation replies
	// to the client before a connector necessarily observes its cancelled context.
	CallMCPInScope(ctx context.Context, scopeID, server, tool string, args map[string]any) (string, bool, error)
	CloseScope(ctx context.Context, scopeID string) error
}

// ReloadBackend optionally extends Backend with child-owned configuration
// reload. The request intentionally carries no server definitions: the backend
// re-reads them where connector credentials live and returns public metadata.
type ReloadBackend interface {
	Reload(ctx context.Context) (*ReloadResult, error)
}

// Server answers mcpbroker requests by running each against a Backend — the end
// that holds the connector secrets and the MCP subprocesses. A Client in another
// process reaches it over a connection.
type Server struct {
	backend Backend
}

const (
	errBrokerCallFailed       = "mcpbroker: credential-owner call failed"
	errBrokerDiscoveryFailed  = "mcpbroker: credential-owner discovery failed"
	errBrokerScopeOpenFailed  = "mcpbroker: credential-owner scope open failed"
	errBrokerScopeCloseFailed = "mcpbroker: credential-owner scope close failed"
	errBrokerReloadFailed     = "mcpbroker: credential-owner reload failed"
)

// NewServer returns a Server that dispatches requests to backend.
func NewServer(backend Backend) *Server {
	return &Server{backend: backend}
}

// Serve reads requests from conn and answers them until conn closes, the decoder
// hits a fatal error, or ctx is cancelled. Each call runs in its own goroutine
// (with a context the matching methodCancel — or ctx — can cancel) so one slow MCP
// call never blocks other requests multiplexed on the same connection. Responses
// are written under a mutex because a json.Encoder is not safe for concurrent use.
//
// Serve closes conn when ctx is cancelled (the only way to unblock a parked
// Decode); it otherwise leaves the conn to the caller. It returns nil on a clean
// peer hangup (EOF) or ctx cancellation, and the decode error otherwise.
func (s *Server) Serve(ctx context.Context, conn io.ReadWriteCloser) error {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var writeMu sync.Mutex
	write := func(resp response) {
		writeMu.Lock()
		defer writeMu.Unlock()
		// Best-effort: a write error means the conn is gone, which the read loop
		// will observe too — no separate error path needed.
		_ = enc.Encode(resp)
	}

	var mu sync.Mutex
	inflight := make(map[uint64]context.CancelFunc)
	var wg sync.WaitGroup

	// Unblock a parked Decode when ctx is cancelled by closing the conn.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			// Drain: cancel everything still running, then wait so we never leak a
			// goroutine writing to a dead conn past Serve's return.
			mu.Lock()
			for _, cancel := range inflight {
				cancel()
			}
			mu.Unlock()
			wg.Wait()
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}

		switch req.Method {
		case methodPing:
			write(response{ID: req.ID})

		case methodCancel:
			// The frame's ID names the in-flight request to stop.
			mu.Lock()
			if cancel, ok := inflight[req.ID]; ok {
				cancel()
			}
			mu.Unlock()

		case methodCall:
			callCtx, cancel := context.WithCancel(ctx)
			mu.Lock()
			inflight[req.ID] = cancel
			mu.Unlock()
			wg.Add(1)
			go func(req request) {
				defer recoverBackendPanic("mcpbroker.call", req.ID, write)
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, req.ID)
					mu.Unlock()
					cancel()
				}()
				// Args arrives nil when the peer's empty map crossed the wire
				// (args,omitempty drops it); forward an object, not null.
				if req.Args == nil {
					req.Args = map[string]any{}
				}
				var text string
				var isErr bool
				var err error
				if req.Scope == "" {
					text, isErr, err = s.backend.CallMCP(callCtx, req.Server, req.Tool, req.Args)
				} else if scoped, ok := s.backend.(ScopedBackend); ok {
					text, isErr, err = scoped.CallMCPInScope(callCtx, req.Scope, req.Server, req.Tool, req.Args)
				} else {
					write(response{ID: req.ID, Err: "mcpbroker: backend does not support scoped sessions"})
					return
				}
				resp := response{ID: req.ID}
				if err != nil {
					// Operational errors can embed connector stderr, URLs, headers,
					// or provider detail. Discard both the error and any partial text;
					// only successful tool output may cross the credential boundary.
					// The detail is logged host-side first (redacted) so the failure is
					// diagnosable here instead of nowhere — see logMasked.
					logMasked("tool call", req.Server+"."+req.Tool, err)
					resp.Err = errBrokerCallFailed
				} else {
					resp.Text = text
					resp.IsError = isErr
				}
				write(resp)
			}(req)

		case methodListTools:
			wg.Add(1)
			go func(req request) {
				defer recoverBackendPanic("mcpbroker.list_tools", req.ID, write)
				defer wg.Done()
				tools, err := s.backend.ListTools(ctx)
				resp := response{ID: req.ID}
				if err != nil {
					logMasked("tool discovery", "", err)
					resp.Err = errBrokerDiscoveryFailed
				} else {
					resp.Tools = tools
				}
				write(resp)
			}(req)

		case methodListAccounts:
			wg.Add(1)
			go func(req request) {
				defer recoverBackendPanic("mcpbroker.list_accounts", req.ID, write)
				defer wg.Done()
				accounts, err := s.backend.ListAccounts(ctx, req.Server, req.BaseVars)
				resp := response{ID: req.ID}
				if err != nil {
					logMasked("account discovery", req.Server, err)
					resp.Err = errBrokerDiscoveryFailed
				} else {
					resp.Accounts = accounts
				}
				write(resp)
			}(req)

		case methodOpenScope:
			callCtx, cancel := context.WithCancel(ctx)
			mu.Lock()
			inflight[req.ID] = cancel
			mu.Unlock()
			wg.Add(1)
			go func(req request) {
				defer recoverBackendPanic("mcpbroker.scope_open", req.ID, write)
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, req.ID)
					mu.Unlock()
					cancel()
				}()
				resp := response{ID: req.ID}
				scoped, ok := s.backend.(ScopedBackend)
				if !ok {
					resp.Err = "mcpbroker: backend does not support scoped sessions"
					write(resp)
					return
				}
				resp.Scope, resp.Tools, resp.Skipped, resp.Err = openScope(callCtx, scoped, req.ScopeSpec)
				write(resp)
			}(req)

		case methodCloseScope:
			if req.Scope == "" {
				write(response{ID: req.ID, Err: "mcpbroker: scope_close requires a scope ID"})
				continue
			}
			callCtx, cancel := context.WithCancel(ctx)
			mu.Lock()
			inflight[req.ID] = cancel
			mu.Unlock()
			wg.Add(1)
			go func(req request) {
				defer recoverBackendPanic("mcpbroker.scope_close", req.ID, write)
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, req.ID)
					mu.Unlock()
					cancel()
				}()
				resp := response{ID: req.ID}
				if scoped, ok := s.backend.(ScopedBackend); ok {
					if err := scoped.CloseScope(callCtx, req.Scope); err != nil {
						logMasked("scope close", req.Scope, err)
						resp.Err = errBrokerScopeCloseFailed
					}
				} else {
					resp.Err = "mcpbroker: backend does not support scoped sessions"
				}
				write(resp)
			}(req)

		case methodReload:
			callCtx, cancel := context.WithCancel(ctx)
			mu.Lock()
			inflight[req.ID] = cancel
			mu.Unlock()
			wg.Add(1)
			go func(req request) {
				defer recoverBackendPanic("mcpbroker.reload", req.ID, write)
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, req.ID)
					mu.Unlock()
					cancel()
				}()
				resp := response{ID: req.ID}
				if reloadable, ok := s.backend.(ReloadBackend); ok {
					result, err := reloadable.Reload(callCtx)
					switch {
					case err != nil:
						// Reload errors can embed resolved URLs, headers, or subprocess
						// environment values. Unlike ordinary tool output, none of that is
						// permitted to cross from the credential owner to the parent.
						logMasked("reload", "", err)
						resp.Err = errBrokerReloadFailed
					case result == nil:
						resp.Err = "mcpbroker: backend returned an empty reload result"
					default:
						resp.Reload = result
					}
				} else {
					resp.Err = "mcpbroker: backend does not support reload"
				}
				write(resp)
			}(req)

		default:
			write(response{ID: req.ID, Err: "mcpbroker: unknown method " + string(req.Method)})
		}
	}
}

func openScope(ctx context.Context, backend ScopedBackend, spec ScopeSpec) (string, []ToolDescriptor, []string, string) {
	if err := validateScopeSpec(spec); err != nil {
		return "", nil, nil, err.Error()
	}
	id, tools, skipped, err := backend.OpenScope(ctx, spec)
	if err != nil {
		logMasked("scope open", "", err)
		return "", nil, nil, errBrokerScopeOpenFailed
	}
	if id == "" {
		return "", nil, nil, "mcpbroker: backend returned an empty scope ID"
	}
	return id, tools, skipped, ""
}

func validateScopeSpec(spec ScopeSpec) error {
	if spec.Remote == nil {
		return nil
	}
	if len(spec.Selection) != 0 || spec.TaskID != "" || spec.Workspace != "" {
		return errors.New("mcpbroker: remote scope cannot include bundle scope fields")
	}
	if strings.TrimSpace(spec.Remote.UserEmail) == "" {
		return errors.New("mcpbroker: remote scope requires a user email")
	}
	if !spec.Remote.FilterEnabled && len(spec.Remote.Enabled) != 0 {
		return errors.New("mcpbroker: remote enabled names require filterEnabled")
	}
	return nil
}

// recoverBackendPanic contains a panic in one broker request and, critically,
// completes that request with a value-free error. Recovering without replying
// leaves the parent Client blocked until its context expires. The incident ID
// correlates the in-band error with safe's structured event while the recovered
// value (which may contain connector material) never crosses the broker pipe.
func recoverBackendPanic(location string, requestID uint64, write func(response)) {
	if recovered := recover(); recovered != nil {
		event := safe.EmitPanicWithMetadata(safe.PanicMetadata{
			Location: location,
			Boundary: "mcpbroker",
		}, recovered, nil)
		write(response{
			ID:  requestID,
			Err: fmt.Sprintf("mcpbroker backend panic (incident %s)", event.IncidentID),
		})
	}
}
