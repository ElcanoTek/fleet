package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"
)

// Cooperative stop of a running tools/call (docs/APPROVAL-STOP.md).
//
// A caller that may want to stop a long call part way attaches a stop signal
// with WithStop. Server.callTool then marks the request with
// `_meta["fleet/cooperative_cancel"] = true`, which tells the server that
// this client, unlike the MCP spec's default, will keep reading for the
// call's response after it cancels. When the signal fires, the transport
// sends the spec's `notifications/cancelled` for the call's request id, with
// reason StopReasonByUser, and goes on waiting for the response under the
// call's own deadline. A server that honours it finishes its current unit of
// work and answers normally (with a stopped reason in its own result); a
// server that hard-cancels answers an error; one that sends nothing runs into
// the call's deadline, exactly as a call that never stopped.
//
// Like WithProgress, the signal is a context value and does not cross a
// process boundary: the out-of-process broker sends the stop on its own wire
// and re-attaches a signal on the side that reaches Server.callTool.

// StopReasonByUser is the `reason` of the notifications/cancelled fleet sends
// when a person pressed Stop.
const StopReasonByUser = "stopped_by_user"

// CooperativeCancelMetaKey is the `_meta` key a stoppable call carries.
const CooperativeCancelMetaKey = RequestMetaPrefix + "cooperative_cancel"

// ErrStoppedBeforeSend is a stoppable call whose stop signal had already
// fired when its transport was about to send the request: nothing was sent,
// so nothing ran. Every transport checks it at its own send boundary.
var ErrStoppedBeforeSend = errors.New("stopped before the call was sent to its server; nothing ran")

// stoppedBeforeSend reports whether ctx's stop signal has already fired.
func stoppedBeforeSend(ctx context.Context) bool {
	stop := StopSignal(ctx)
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// StoppedBeforeSend is stoppedBeforeSend for the out-of-process broker,
// which sends the call on its own wire.
func StoppedBeforeSend(ctx context.Context) bool { return stoppedBeforeSend(ctx) }

// stopNotifyTimeout bounds sending the cancellation over HTTP, which is a
// request of its own.
const stopNotifyTimeout = 10 * time.Second

type stopSignalKey struct{}

// WithStop attaches a stop signal to ctx: closing stop asks the server to
// stop the call made under ctx. A nil channel attaches nothing.
func WithStop(ctx context.Context, stop <-chan struct{}) context.Context {
	if stop == nil {
		return ctx
	}
	return context.WithValue(ctx, stopSignalKey{}, stop)
}

// StopSignal returns the stop signal WithStop attached to ctx, or nil. The
// out-of-process broker reads it to forward a stop.
func StopSignal(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(stopSignalKey{}).(<-chan struct{})
	return ch
}

// cancelledNotification is the JSON-RPC notifications/cancelled message for
// request id, ready to write.
func cancelledNotification(id int) []byte {
	b, _ := json.Marshal(map[string]any{
		jsonRPCFieldJSONRPC: jsonRPCVersion,
		"method":            "notifications/cancelled",
		"params":            map[string]any{"requestId": id, "reason": StopReasonByUser},
	})
	return b
}

// notifyStopHTTP sends notifications/cancelled for request id once stop
// fires and the call's request has been written (wrote), and sends it again
// on a backoff until the call returns (done). Over HTTP the cancellation is a
// request of its own, on another connection, so the server can read it
// before it has registered the call it names, and a server ignores a
// cancellation for a request it does not know (the spec says so). Repeating
// it is harmless (a cancellation of a request already being cancelled
// changes nothing) and makes sure one lands once the call is in flight.
func (t *HTTPTransport) notifyStopHTTP(ctx context.Context, stop, wrote, done <-chan struct{}, id int) {
	for _, gate := range []<-chan struct{}{stop, wrote} {
		select {
		case <-gate:
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
	wait := stopResendFirst
	for sent := 0; sent < stopResendMax; sent++ {
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopNotifyTimeout)
		err := t.Notify(nctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": StopReasonByUser})
		cancel()
		if err != nil {
			log.Printf("MCP http: could not send the stop for request %d: %v", id, err)
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-done:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		}
		wait = min(wait*2, stopResendMaxInterval)
	}
}

// The HTTP stop's resend schedule: the first repeat after stopResendFirst,
// doubling up to stopResendMaxInterval, at most stopResendMax sends. Vars,
// not consts: tests shorten them.
var (
	stopResendFirst       = 500 * time.Millisecond
	stopResendMaxInterval = 5 * time.Second
	stopResendMax         = 30
)
