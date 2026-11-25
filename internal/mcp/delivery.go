package mcp

import "errors"

// A failed tools/call can mean two very different things to a caller whose
// tool has a side effect (send_email, a record create): the request never
// reached the server, so nothing ran and a retry is safe; or it may have
// reached the server, which may have acted before the failure, so a blind
// retry can do the thing twice. The error text cannot be trusted to tell
// them apart: it can quote a server's own output, and a wrapped error can
// carry a phrase that belongs to a different request (a restart's
// initialize, say). So the call sites that know mark the error itself, and
// callers ask RequestNotSent.

// deliveryError marks whether the tools/call request behind err reached its
// server.
type deliveryError struct {
	err     error
	notSent bool
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
	return &deliveryError{err: err, notSent: true}
}

// markDelivery marks err as not sent when notSent, and otherwise as a
// request that may have reached its server. The second form matters when err
// wraps an inner error marked not sent that is about a different request: the
// outer mark is the one RequestNotSent reads.
func markDelivery(err error, notSent bool) error {
	if err == nil {
		return nil
	}
	return &deliveryError{err: err, notSent: notSent}
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
		return d.notSent
	}
	return errors.Is(err, ErrStoppedBeforeSend)
}
