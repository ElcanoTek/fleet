package mcpbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Server answers mcpbroker requests by running each against a host-side
// agentcore.MCPBroker — in production the credentialed localMCPBroker. It is the
// end that holds the connector secrets and the MCP subprocesses; a Client in
// another process reaches it over a connection. The Server adds no policy of its
// own: it is a transport shell around the SAME MCPBroker the in-process loop uses,
// which is the point (no second governed call path, issue #167).
type Server struct {
	broker agentcore.MCPBroker
}

// NewServer returns a Server that dispatches calls to broker.
func NewServer(broker agentcore.MCPBroker) *Server {
	return &Server{broker: broker}
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
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, req.ID)
					mu.Unlock()
					cancel()
				}()
				text, isErr, err := s.broker.CallMCP(callCtx, req.Server, req.Tool, req.Args)
				resp := response{ID: req.ID, Text: text, IsError: isErr}
				if err != nil {
					resp.Err = err.Error()
				}
				write(resp)
			}(req)

		default:
			write(response{ID: req.ID, Err: "mcpbroker: unknown method " + string(req.Method)})
		}
	}
}
