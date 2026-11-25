package mcp

import (
	"errors"
	"net/http"
)

// A failed tools/call can mean three different things to a caller whose tool
// has a side effect (send_email, a record create):
//
//   - the request never reached the server, so nothing ran and a retry is
//     safe (RequestNotSent);
//   - the server received it and answered with a refusal — an HTTP 4xx —
//     so it did not act, and a retry is as safe as the first attempt
//     (RequestRefused);
//   - anything else: it may have reached the server, which may have acted
//     before the failure, so a blind retry can do the thing twice.
//
// The error text cannot be trusted to tell them apart: it can quote a
// server's own output, and a wrapped error can carry a phrase that belongs to
// a different request (a restart's initialize, say). So the call sites that
// know mark the error itself, and callers ask RequestNotSent and
// RequestRefused.

// delivery is what a failed tools/call's mark says about its request.
type delivery int

const (
	deliveryUnknown delivery = iota // may have reached the server and run
	deliveryNotSent                 // never reached the server
	deliveryRefused                 // reached the server, which refused it
)

// deliveryError marks what happened to the tools/call request behind err.
type deliveryError struct {
	err   error
	state delivery
}

func (e *deliveryError) Error() string { return e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }

// MarkNotSent marks err as a failure that provably happened before the tool
// request reached its server: nothing ran. The out-of-process broker uses it
// to carry the child's answer across the process boundary.
func MarkNotSent(err error) error {
	if err == nil {
		return nil
	}
	return &deliveryError{err: err, state: deliveryNotSent}
}

// MarkRefused marks err as the server's refusal of a request it received
// (an HTTP 4xx): it did not act. The out-of-process broker uses it to carry
// the child's answer across the process boundary, where the typed
// HTTPStatusError does not survive.
func MarkRefused(err error) error {
	if err == nil {
		return nil
	}
	return &deliveryError{err: err, state: deliveryRefused}
}

// markDelivery marks err with what is known about the request behind it:
// not sent when notSent, refused when err carries the server's 4xx answer
// (RequestRefused), and otherwise a request that may have reached its server.
// The last form matters when err wraps an inner mark about a different
// request: the outer mark is the one RequestNotSent and RequestRefused read.
func markDelivery(err error, notSent bool) error {
	if err == nil {
		return nil
	}
	state := deliveryUnknown
	switch {
	case notSent:
		state = deliveryNotSent
	case RequestRefused(err):
		state = deliveryRefused
	}
	return &deliveryError{err: err, state: state}
}

// RequestNotSent reports whether err says the tool request provably never
// reached its server. The outermost mark decides (errors.As finds it first),
// so a wrapper can overrule a mark it wraps. An error with no mark is not
// known to be undelivered and reports false — the answer a caller deciding
// whether a retry could act twice must lean towards. ErrStoppedBeforeSend is
// not sent by definition.
func RequestNotSent(err error) bool {
	var d *deliveryError
	if errors.As(err, &d) {
		return d.state == deliveryNotSent
	}
	return errors.Is(err, ErrStoppedBeforeSend)
}

// RequestRefused reports whether err says the server received the tool
// request and refused it, so it did not act: its mark, or, for an error no
// call site has marked yet, the typed HTTP 4xx it carries. A 4xx means the
// server received the request, judged it and refused it — bad arguments
// (400, 422), a refused credential or permission (401, 403), a rate limit
// (429). A 5xx is not a refusal: a gateway that timed out (502, 504) or a
// server that failed mid-request may have acted. Like RequestNotSent, the
// outermost mark decides.
func RequestRefused(err error) bool {
	var d *deliveryError
	if errors.As(err, &d) {
		return d.state == deliveryRefused
	}
	var hs *HTTPStatusError
	return errors.As(err, &hs) && hs.StatusCode >= http.StatusBadRequest && hs.StatusCode < http.StatusInternalServerError
}
