package mcp

import (
	"context"
	"sync"
)

// ArgumentHold tracks whether anything a call started may still hold the
// call's arguments after the call itself returned. The MCP workspace-file
// budget (ADR-0088) charges a call's base64 arguments until they are no longer
// live, and an out-of-process dispatch that the caller cancelled keeps them
// alive in the broker child until the child answers. A transport in that
// position takes a retention (RetainArguments) and ends it when it has
// provably let go; the hold's release runs once the caller is Done AND every
// retention has ended.
type ArgumentHold struct {
	mu      sync.Mutex
	pending int
	done    bool
	release func()
}

type argumentHoldKey struct{}

// WithArgumentHold returns ctx carrying a new hold whose release runs after
// Done and every retention taken under ctx have ended. release may be nil.
func WithArgumentHold(ctx context.Context, release func()) (context.Context, *ArgumentHold) {
	h := &ArgumentHold{release: release}
	return context.WithValue(ctx, argumentHoldKey{}, h), h
}

// RetainArguments records that the caller's arguments outlive the call that
// is about to return. The returned func ends the retention; it is idempotent.
// Without a hold on ctx it is a no-op.
func RetainArguments(ctx context.Context) func() {
	h, _ := ctx.Value(argumentHoldKey{}).(*ArgumentHold)
	if h == nil {
		return func() {}
	}
	h.mu.Lock()
	h.pending++
	h.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { h.end(false) }) }
}

// Done marks the caller finished with the call. It is idempotent.
func (h *ArgumentHold) Done() {
	h.end(true)
}

func (h *ArgumentHold) end(caller bool) {
	h.mu.Lock()
	if caller {
		if h.done {
			h.mu.Unlock()
			return
		}
		h.done = true
	} else {
		h.pending--
	}
	fire := h.done && h.pending == 0 && h.release != nil
	release := h.release
	if fire {
		h.release = nil
	}
	h.mu.Unlock()
	if fire {
		release()
	}
}
