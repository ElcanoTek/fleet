package mcp

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// MCP progress notifications (docs/APPROVAL-PROGRESS.md).
//
// A caller that wants to hear how a long tool call is going attaches a sink
// with WithProgress. Server.callTool then sends the request with
// `_meta.progressToken` (the MCP spec's opt-in), and the transport hands every
// `notifications/progress` carrying that token to the sink while it waits for
// the call's response. A call without a sink sends exactly the request it
// always sent, and notifications are skipped as before.
//
// The sink runs on the transport's read path, under the transport's lock, so
// it must return quickly and must not call back into the same server. Wrap a
// slow consumer in ThrottleProgress. Like WithCallTimeout, a sink is a context
// value and does not cross a process boundary: the out-of-process broker
// forwards updates on its own wire and re-attaches a sink on the side that
// reaches Server.callTool.

// ProgressUpdate is one notifications/progress for a tool call.
type ProgressUpdate struct {
	// Progress is how far the call has got. It only increases, per the spec;
	// fleet does not enforce that.
	Progress float64 `json:"progress"`
	// Total is the amount of work when the server knows it, 0 otherwise.
	Total float64 `json:"total,omitempty"`
	// Message is the server's short description of the current step, cut to
	// MaxProgressMessageRunes.
	Message string `json:"message,omitempty"`
}

// MaxProgressMessageRunes bounds a progress message as it is read, so a
// server cannot push an unbounded string through every hop.
const MaxProgressMessageRunes = 500

type progressSinkKey struct{}

// progressTokenKey carries the token callTool minted for this call, so the
// transport only delivers notifications meant for it.
type progressTokenKey struct{}

var progressTokenSeq atomic.Uint64

// WithProgress attaches a progress sink to ctx. A nil fn attaches nothing.
func WithProgress(ctx context.Context, fn func(ProgressUpdate)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressSinkKey{}, fn)
}

// ProgressSink returns the sink WithProgress attached to ctx, or nil. The
// out-of-process broker reads it to decide whether to ask the child for
// progress frames.
func ProgressSink(ctx context.Context) func(ProgressUpdate) {
	fn, _ := ctx.Value(progressSinkKey{}).(func(ProgressUpdate))
	return fn
}

// withProgressToken mints this call's progress token and returns the params
// `_meta` to send plus the context the transport reads it from. ok is false
// when ctx carries no sink: the request is sent unchanged.
func withProgressToken(ctx context.Context) (context.Context, map[string]any, bool) {
	if ProgressSink(ctx) == nil {
		return ctx, nil, false
	}
	token := "fleet-" + strconv.FormatUint(progressTokenSeq.Add(1), 10)
	return context.WithValue(ctx, progressTokenKey{}, token), map[string]any{"progressToken": token}, true
}

// deliverProgress hands one server-initiated message to ctx's sink when it is
// a notifications/progress for this call's token. Anything else (another
// notification, another call's token, a malformed or non-finite value) is
// ignored, exactly as every notification was before.
func deliverProgress(ctx context.Context, method string, params json.RawMessage) {
	if method != "notifications/progress" || len(params) == 0 {
		return
	}
	fn := ProgressSink(ctx)
	token, _ := ctx.Value(progressTokenKey{}).(string)
	if fn == nil || token == "" {
		return
	}
	var p struct {
		ProgressToken json.RawMessage `json:"progressToken"`
		Progress      *float64        `json:"progress"`
		Total         *float64        `json:"total"`
		Message       string          `json:"message"`
	}
	if json.Unmarshal(params, &p) != nil || p.Progress == nil {
		return
	}
	var got string
	if json.Unmarshal(p.ProgressToken, &got) != nil || got != token {
		return
	}
	u := ProgressUpdate{Progress: *p.Progress, Message: truncateRunes(p.Message, MaxProgressMessageRunes)}
	if p.Total != nil {
		u.Total = *p.Total
	}
	if !finiteNonNegative(u.Progress) || !finiteNonNegative(u.Total) {
		return
	}
	fn(u)
}

// deliverProgressJSON is deliverProgress for one whole JSON-RPC message (an
// SSE event's data).
func deliverProgressJSON(ctx context.Context, data []byte) {
	if ProgressSink(ctx) == nil {
		return
	}
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(data, &msg) != nil {
		return
	}
	deliverProgress(ctx, msg.Method, msg.Params)
}

func finiteNonNegative(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// ThrottleProgress wraps fn so it receives at most one update per interval.
// The latest update is never lost: one that arrives inside the interval is
// held and delivered when the interval ends, replacing any update held before
// it. stop drops a held update and stops the timer; nothing reaches fn after
// stop returns. Updates are delivered one at a time, in order, so fn need not
// be safe for concurrent use. push never blocks on the interval.
func ThrottleProgress(interval time.Duration, fn func(ProgressUpdate)) (push func(ProgressUpdate), stop func()) {
	var (
		mu      sync.Mutex
		last    time.Time
		pending *ProgressUpdate
		timer   *time.Timer
		stopped bool
	)
	flush := func() {
		mu.Lock()
		defer mu.Unlock()
		timer = nil
		if stopped || pending == nil {
			return
		}
		u := *pending
		pending = nil
		last = time.Now()
		fn(u)
	}
	push = func(u ProgressUpdate) {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		if wait := interval - time.Since(last); wait > 0 {
			pending = &u
			if timer == nil {
				timer = time.AfterFunc(wait, flush)
			}
			return
		}
		pending = nil
		last = time.Now()
		fn(u)
	}
	stop = func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		pending = nil
		if timer != nil {
			timer.Stop()
			timer = nil
		}
	}
	return push, stop
}
