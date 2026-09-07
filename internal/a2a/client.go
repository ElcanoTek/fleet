// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package a2a

// The OUTBOUND half of the bridge (#1368 — #1279 Phase 3): the thin JSON-RPC
// client fleet's a2a peer tools speak to a remote A2A server with. It shapes
// bytes and nothing else — which peers exist, who may call them, and what the
// model gets to see are decided by the bundle and the tool layer
// (internal/mcp/a2atool.go), never here.
//
// Why not the SDK's a2aclient: its JSON-RPC encoding lives in a Go-internal
// package (unimportable piecemeal), and its defaults do not fit fleet's
// posture — a 3-minute whole-request timeout, file:// agent-card resolution,
// and a discarded HTTP error body (fleet's own server puts the remediation
// text there). The valuable part — the wire types, above all the
// StreamResponse oneof — is the types-only import this package already
// carries, so the client reuses them exactly as the server does.
//
// Outbound posture: every connection goes through mcpoauth's resolve-then-dial
// SSRF guard and refuses redirects unconditionally (a 30x must never carry a
// peer credential to another origin). ClientOptions.AllowPrivate relaxes the
// dial guard for dev/test rigs against loopback peers — never the redirect
// refusal. Every response body is read through a hard byte cap.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wire "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ElcanoTek/fleet/internal/mcpoauth"
)

// DepthHeader is the fleet extension header an outbound send carries so a
// chain of fleet deployments delegating to one another cannot loop: the
// caller sends its own delegation depth plus one, the receiving fleet refuses
// values past FLEET_A2A_MAX_DELEGATION_DEPTH and stamps the accepted depth on
// the task it creates. Cooperative by construction — a non-fleet peer drops
// the header and the chain restarts at depth one on the far side.
const DepthHeader = "X-Fleet-A2A-Depth"

const (
	// unaryTimeout bounds one unary call end to end.
	unaryTimeout = 30 * time.Second
	// maxUnaryBody caps a unary response body; a Task carrying inline text
	// artifacts is the largest legitimate answer and stays far below this.
	maxUnaryBody = 2 << 20
	// maxStreamBytes caps one SSE subscription's total bytes.
	maxStreamBytes = 1 << 20
	// maxEventBytes caps one SSE event's data payload.
	maxEventBytes = 256 << 10
	// errorBodyPreview bounds how much of a non-200 body is carried back.
	errorBodyPreview = 512
	// streamHeaderTimeout bounds how long a peer may take to answer the
	// subscription's response headers before the stream starts.
	streamHeaderTimeout = 30 * time.Second
	// streamSettleGrace is how long WaitForUpdate keeps reading after an
	// artifact (or message) event before returning it. Servers emit a task's
	// artifacts and THEN its terminal status, back to back (fleet's own does:
	// "artifacts first, then the terminal status — generation order"), so
	// returning on the artifact alone would hand the caller a WORKING snapshot
	// with finished artifacts and cost it a second wait for the status that
	// was milliseconds behind.
	streamSettleGrace = time.Second
)

// ClientOptions tunes NewClient.
type ClientOptions struct {
	// AllowPrivate disables the SSRF dial guard (never the redirect refusal)
	// so a peer on loopback / a private network can be reached — the
	// FLEET_A2A_CLIENT_ALLOW_PRIVATE dev/test posture.
	AllowPrivate bool
}

// Client speaks JSON-RPC to one remote A2A server.
type Client struct {
	rpcURL  string
	headers map[string]string
	unary   *http.Client
	stream  *http.Client
	nextID  atomic.Int64
}

// NewClient builds a client for the peer at rpcURL. headers are the resolved
// credential/static headers applied to every request (their values live only
// in this process and are never echoed into results or errors).
func NewClient(rpcURL string, headers map[string]string, opts ClientOptions) *Client {
	c := &Client{rpcURL: rpcURL, headers: make(map[string]string, len(headers))}
	for k, v := range headers {
		c.headers[k] = v
	}
	if opts.AllowPrivate {
		refuse := func(*http.Request, []*http.Request) error {
			return errors.New("a2a client: redirects are refused")
		}
		tr, ok := http.DefaultTransport.(*http.Transport)
		streamTr := http.DefaultTransport
		if ok {
			cloned := tr.Clone()
			cloned.ResponseHeaderTimeout = streamHeaderTimeout
			streamTr = cloned
		}
		c.unary = &http.Client{Timeout: unaryTimeout, CheckRedirect: refuse}
		c.stream = &http.Client{Transport: streamTr, CheckRedirect: refuse}
		return c
	}
	c.unary = mcpoauth.SafeHTTPClient(unaryTimeout)
	c.stream = mcpoauth.SafeStreamingHTTPClient()
	return c
}

// clientRequest is the outbound envelope: Request's inverse typing (Params is
// marshalled inline as the bare wire request struct, no wrapper — spec §9.1).
type clientRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// clientResponse is the inbound envelope: Response with Result left raw.
type clientResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ErrorObject    `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error the peer answered with. errors.Is against the
// wire sentinels works through SentinelFor for the spec-defined codes.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return fmt.Sprintf("A2A error %d: %s", e.Code, e.Message) }

// Unwrap yields the wire sentinel for a spec-defined code, nil otherwise.
func (e *RPCError) Unwrap() error { return SentinelFor(e.Code) }

// Reason is the spec's ErrorInfo reason token for the code ("" when unknown).
func (e *RPCError) Reason() string {
	if s := SentinelFor(e.Code); s != nil {
		return wire.ErrorReason(s)
	}
	return ""
}

// HTTPError is a non-200 transport answer. Fleet's own server answers an auth
// failure this way (401, remediation text in the body), so the body preview
// is kept rather than discarded.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("peer answered HTTP %d: %s", e.Status, e.Body)
}

// SendResult is what SendMessage produced: a Task to poll (the normal case,
// and the only shape fleet's own server answers with) or — from a peer that
// answers a message directly — a bare Message with nothing to poll.
type SendResult struct {
	Task    *wire.Task
	Message *wire.Message
}

// SendMessage creates a task on the peer (taskID empty) or answers the
// question an INPUT_REQUIRED task asked (taskID set). It never blocks on the
// outcome — returnImmediately is always true; the tool layer polls or waits.
// depth is THIS run's delegation depth; the request carries depth+1.
func (c *Client) SendMessage(ctx context.Context, text string, taskID wire.TaskID, depth int) (SendResult, error) {
	msg := wire.NewMessage(wire.MessageRoleUser, wire.NewTextPart(text))
	msg.TaskID = taskID
	params := wire.SendMessageRequest{
		Message: msg,
		Config:  &wire.SendMessageConfig{ReturnImmediately: true},
	}
	raw, err := c.call(ctx, MethodSendMessage, params, map[string]string{DepthHeader: strconv.Itoa(depth + 1)})
	if err != nil {
		return SendResult{}, err
	}
	var sr wire.StreamResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return SendResult{}, fmt.Errorf("%s: decode result: %w", MethodSendMessage, err)
	}
	switch ev := sr.Event.(type) {
	case *wire.Task:
		return SendResult{Task: ev}, nil
	case *wire.Message:
		return SendResult{Message: ev}, nil
	}
	return SendResult{}, fmt.Errorf("%s: result carried neither a task nor a message", MethodSendMessage)
}

// GetTask fetches the peer's current snapshot of a task.
func (c *Client) GetTask(ctx context.Context, id wire.TaskID) (*wire.Task, error) {
	return c.taskCall(ctx, MethodGetTask, wire.GetTaskRequest{ID: id})
}

// Cancel asks the peer to cancel a task and returns the resulting snapshot.
func (c *Client) Cancel(ctx context.Context, id wire.TaskID) (*wire.Task, error) {
	return c.taskCall(ctx, MethodCancelTask, wire.CancelTaskRequest{ID: id})
}

func (c *Client) taskCall(ctx context.Context, method string, params any) (*wire.Task, error) {
	raw, err := c.call(ctx, method, params, nil)
	if err != nil {
		return nil, err
	}
	var t wire.Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("%s: decode result: %w", method, err)
	}
	return &t, nil
}

// WaitResult is what WaitForUpdate observed.
type WaitResult struct {
	// Task is the freshest snapshot; nil only when the stream carried none.
	Task *wire.Task
	// Message is an agent message the stream carried in place of a task
	// change (peers that converse rather than run tasks).
	Message *wire.Message
	// Changed reports that a status/artifact update (or message) arrived
	// after the opening snapshot.
	Changed bool
	// Terminal reports that the task is in a terminal state.
	Terminal bool
	// TimedOut reports the wait elapsed with no change after the snapshot.
	TimedOut bool
	// StreamClosed reports the peer closed the stream before a terminal state
	// (fleet's server does this at its stream-lifetime bound).
	StreamClosed bool
}

// WaitForUpdate subscribes to the task's event stream (SubscribeToTask, SSE)
// and returns on the first change after the opening snapshot, on a terminal
// state, or when wait elapses — whichever comes first. A peer that refuses the
// subscription because the task is already terminal (-32004, fleet's own
// behavior) is answered with a GetTask snapshot instead.
func (c *Client) WaitForUpdate(ctx context.Context, id wire.TaskID, wait time.Duration) (WaitResult, error) {
	if wait <= 0 {
		wait = time.Second
	}
	// parent is the CALLER's context. The wait-scoped deadline below is the
	// stream's budget, not the caller's: the GetTask fallback after the
	// stream ends must escape the expired wait deadline but still honor the
	// caller's cancellation (WithoutCancel(ctx) stripped both).
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	body, err := json.Marshal(clientRequest{
		JSONRPC: "2.0", ID: c.nextID.Add(1), Method: MethodSubscribeToTask,
		Params: wire.SubscribeToTaskRequest{ID: id},
	})
	if err != nil {
		return WaitResult{}, fmt.Errorf("%s: encode: %w", MethodSubscribeToTask, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL, bytes.NewReader(body))
	if err != nil {
		return WaitResult{}, fmt.Errorf("%s: build request: %w", MethodSubscribeToTask, err)
	}
	c.applyHeaders(req)
	req.Header.Set("Accept", "text/event-stream, application/json")
	resp, err := c.stream.Do(req)
	if err != nil {
		return WaitResult{}, fmt.Errorf("%s: %w", MethodSubscribeToTask, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := readCapped(resp.Body, errorBodyPreview)
		return WaitResult{}, &HTTPError{Status: resp.StatusCode, Body: preview(raw)}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// A plain JSON-RPC answer instead of a stream: the peer refused the
		// subscription. Already-terminal is the expected refusal — the
		// outcome is one GetTask away.
		raw, err := readCapped(resp.Body, maxUnaryBody)
		if err != nil {
			return WaitResult{}, fmt.Errorf("%s: %w", MethodSubscribeToTask, err)
		}
		var env clientResponse
		if err := json.Unmarshal(raw, &env); err != nil {
			return WaitResult{}, fmt.Errorf("%s: peer did not open an event stream and answered a non-JSON-RPC body", MethodSubscribeToTask)
		}
		if env.Error == nil {
			return WaitResult{}, fmt.Errorf("%s: peer did not open an event stream", MethodSubscribeToTask)
		}
		if env.Error.Code != CodeFor(wire.ErrUnsupportedOperation) {
			return WaitResult{}, &RPCError{Code: env.Error.Code, Message: env.Error.Message}
		}
		t, err := c.GetTask(ctx, id)
		if err != nil {
			return WaitResult{}, err
		}
		return WaitResult{Task: t, Terminal: t.Status.State.Terminal()}, nil
	}

	// After the first artifact/message event the read is bounded by the
	// settle grace instead of the caller's wait: cancelling the request
	// context is what unblocks the body read.
	var (
		settleOnce  sync.Once
		settleTimer *time.Timer
	)
	armSettle := func() { settleOnce.Do(func() { settleTimer = time.AfterFunc(streamSettleGrace, cancel) }) }
	res, err := readEventStream(ctx, resp.Body, armSettle)
	if settleTimer != nil {
		// The read has returned; a still-pending grace timer would only fire
		// cancel on an already-finished context. Stop it so a tight poll
		// loop does not accumulate one live timer per call.
		settleTimer.Stop()
	}
	if err != nil {
		return WaitResult{}, err
	}
	if res.Task == nil && res.Message == nil {
		// The stream ended before a snapshot landed (deadline, or a peer that
		// closed at once): the outcome is still one GetTask away. Bound the
		// fallback by the caller's context plus one more wait budget — never
		// WithoutCancel, which would keep polling a peer for a caller that
		// has already gone away.
		if parent.Err() != nil {
			return res, nil
		}
		fctx, fcancel := context.WithTimeout(parent, wait)
		defer fcancel()
		t, err := c.GetTask(fctx, id)
		if err != nil {
			return WaitResult{}, err
		}
		res.Task = t
		res.Terminal = t.Status.State.Terminal()
	}
	return res, nil
}

// readEventStream consumes SSE frames until a status update after the opening
// snapshot, a terminal state, the ctx deadline, or stream close. An artifact
// or message event does not end the read by itself: it arms the settle grace
// (armSettle) so a status update emitted right behind it lands in the same
// result, and the read then ends when the grace expires. Each `data:` payload
// is a full JSON-RPC Response envelope whose result is a StreamResponse oneof
// (the spec's streaming binding, and what fleet's own server emits); `:`
// comment lines are keepalives.
func readEventStream(ctx context.Context, body io.Reader, armSettle func()) (WaitResult, error) {
	var res WaitResult
	limited := &countingReader{r: io.LimitReader(body, maxStreamBytes)}
	reader := bufio.NewReaderSize(limited, 64<<10)
	var data bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				// Blank line dispatches the accumulated event.
				if data.Len() > 0 {
					done, applyErr := applyStreamFrame(&res, data.Bytes())
					data.Reset()
					if applyErr != nil {
						return WaitResult{}, applyErr
					}
					if done {
						return res, nil
					}
					if res.Changed && armSettle != nil {
						armSettle()
					}
				}
			case line[0] == ':':
				// Comment / keepalive.
			case bytes.HasPrefix(line, []byte("data:")):
				payload := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
				if data.Len()+len(payload)+1 > maxEventBytes {
					return WaitResult{}, fmt.Errorf("%s: an event exceeded %d bytes", MethodSubscribeToTask, maxEventBytes)
				}
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(payload)
			}
			// event:/id:/retry: fields carry nothing this reader needs.
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			// The deadline IS the wait budget, not a failure: the caller gets the
			// freshest snapshot and decides whether to wait again. After a
			// change the deadline is the settle grace, not a timeout.
			if !res.Changed {
				res.TimedOut = true
			}
			return res, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if limited.n >= maxStreamBytes {
				return WaitResult{}, fmt.Errorf("%s: the stream exceeded %d bytes", MethodSubscribeToTask, maxStreamBytes)
			}
			// Closure after a terminal status is the completion signal; before
			// one it is the peer's lifetime bound (or a dropped connection).
			if !res.Terminal {
				res.StreamClosed = true
			}
			return res, nil
		}
		return WaitResult{}, fmt.Errorf("%s: read stream: %w", MethodSubscribeToTask, err)
	}
}

// applyStreamFrame folds one decoded event into res; done reports that the
// wait should end now (a status update after the snapshot, or a terminal
// snapshot). Artifact and message events mark the result Changed but leave
// the read open for the settle grace.
func applyStreamFrame(res *WaitResult, frame []byte) (bool, error) {
	var env clientResponse
	if err := json.Unmarshal(frame, &env); err != nil {
		return false, fmt.Errorf("%s: malformed event: %w", MethodSubscribeToTask, err)
	}
	if env.Error != nil {
		return false, &RPCError{Code: env.Error.Code, Message: env.Error.Message}
	}
	var sr wire.StreamResponse
	if err := json.Unmarshal(env.Result, &sr); err != nil {
		return false, fmt.Errorf("%s: malformed event: %w", MethodSubscribeToTask, err)
	}
	switch ev := sr.Event.(type) {
	case *wire.Task:
		// The opening snapshot (the spec's first frame). A terminal one ends
		// the wait; otherwise the wait is for what comes after it.
		res.Task = ev
		res.Terminal = ev.Status.State.Terminal()
		return res.Terminal, nil
	case *wire.TaskStatusUpdateEvent:
		if res.Task == nil {
			res.Task = &wire.Task{ID: ev.TaskID, ContextID: ev.ContextID}
		}
		res.Task.Status = ev.Status
		res.Changed = true
		res.Terminal = ev.Status.State.Terminal()
		return true, nil
	case *wire.TaskArtifactUpdateEvent:
		if res.Task == nil {
			res.Task = &wire.Task{ID: ev.TaskID, ContextID: ev.ContextID}
		}
		upsertArtifact(res.Task, ev)
		res.Changed = true
		return false, nil // keep reading: the status update usually follows at once
	case *wire.Message:
		res.Message = ev
		res.Changed = true
		return false, nil
	}
	return false, nil
}

// upsertArtifact applies an artifact update: append chunks onto an existing
// artifact with the same id when asked, replace it otherwise, add it when new.
func upsertArtifact(t *wire.Task, ev *wire.TaskArtifactUpdateEvent) {
	if ev.Artifact == nil {
		return
	}
	for i, existing := range t.Artifacts {
		if existing != nil && existing.ID == ev.Artifact.ID {
			if ev.Append {
				existing.Parts = append(existing.Parts, ev.Artifact.Parts...)
				return
			}
			t.Artifacts[i] = ev.Artifact
			return
		}
	}
	t.Artifacts = append(t.Artifacts, ev.Artifact)
}

// call performs one unary JSON-RPC exchange and returns the raw result.
func (c *Client) call(ctx context.Context, method string, params any, extra map[string]string) (json.RawMessage, error) {
	body, err := json.Marshal(clientRequest{JSONRPC: "2.0", ID: c.nextID.Add(1), Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("%s: encode: %w", method, err)
	}
	ctx, cancel := context.WithTimeout(ctx, unaryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpcURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", method, err)
	}
	c.applyHeaders(req)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.unary.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := readCapped(resp.Body, maxUnaryBody)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode, Body: preview(raw)}
	}
	var env clientResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: peer answered a non-JSON-RPC body: %s", method, preview(raw))
	}
	if env.Error != nil {
		return nil, &RPCError{Code: env.Error.Code, Message: env.Error.Message}
	}
	// A JSON `null` result is "no result" too: decoding it into a wire.Task
	// yields a zero-valued task (empty id, empty state) that callers would
	// treat as data. Refuse it here, for every unary method.
	if trimmed := bytes.TrimSpace(env.Result); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("%s: peer answered with neither result nor error", method)
	}
	return env.Result, nil
}

// applyHeaders writes the credential/static headers, then the protocol
// headers — in that order, so a bundle cannot override the A2A version or
// the content type the binding requires.
func (c *Client) applyHeaders(req *http.Request) {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(wire.SvcParamVersion, string(wire.Version))
}

// readCapped reads at most max bytes and fails when the body is larger.
func readCapped(r io.Reader, limit int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return raw, nil
}

// preview renders a body excerpt for an error message: control characters
// dropped, length bounded.
func preview(raw []byte) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, string(raw))
	s = strings.TrimSpace(s)
	if len(s) > errorBodyPreview {
		s = s[:errorBodyPreview] + "…"
	}
	return s
}

// countingReader tracks bytes delivered so a LimitReader EOF can be told
// apart from a real stream close.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
