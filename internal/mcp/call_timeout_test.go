package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// deadlineTransport records the deadline each tools/call carried.
type deadlineTransport struct {
	calls     int
	remaining time.Duration
	hasDL     bool
}

func (d *deadlineTransport) Call(ctx context.Context, _ string, _ interface{}) (json.RawMessage, error) {
	d.calls++
	var dl time.Time
	dl, d.hasDL = ctx.Deadline()
	if d.hasDL {
		d.remaining = time.Until(dl)
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
}

func (d *deadlineTransport) Notify(context.Context, string, interface{}) error { return nil }
func (d *deadlineTransport) Close() error                                      { return nil }

// TestCallTool_BudgetStartsAfterServerMutex pins that a WithCallTimeout budget
// starts once the call holds the server mutex: time queued behind another call
// on the same server is not charged to the call's own budget.
func TestCallTool_BudgetStartsAfterServerMutex(t *testing.T) {
	tr := &deadlineTransport{}
	srv := &Server{name: "openx_mcp", transport: tr}

	const budget = 2 * time.Second
	const held = 300 * time.Millisecond
	srv.mu.Lock()
	go func() {
		time.Sleep(held)
		srv.mu.Unlock()
	}()
	if _, err := srv.callTool(WithCallTimeout(context.Background(), budget), "ox_merge_deal_domains", nil); err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if !tr.hasDL {
		t.Fatal("transport call carried no deadline; the budget was not applied")
	}
	// Started at lock acquisition: nearly the whole budget remains. Charged
	// from before the wait, it would be at most budget-held.
	if tr.remaining <= budget-held {
		t.Fatalf("remaining budget at the transport = %v; want > %v (budget must start after the mutex wait)", tr.remaining, budget-held)
	}
}

func TestCallTool_NoBudgetAddsNoDeadline(t *testing.T) {
	tr := &deadlineTransport{}
	srv := &Server{name: "openx_mcp", transport: tr}
	if _, err := srv.callTool(context.Background(), "ox_get_deal", nil); err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if tr.hasDL {
		t.Fatalf("no WithCallTimeout budget, but the transport saw a deadline (%v left)", tr.remaining)
	}
	if got := WithCallTimeout(context.Background(), 0); got.Value(callTimeoutKey{}) != nil {
		t.Fatal("WithCallTimeout(0) must attach nothing")
	}
}

// TestCallTool_ExpiredWhileQueuedIsNotSent pins the fail-safe: a call whose
// caller deadline ran out while it waited for the server mutex never reaches
// the transport.
func TestCallTool_ExpiredWhileQueuedIsNotSent(t *testing.T) {
	tr := &deadlineTransport{}
	srv := &Server{name: "openx_mcp", transport: tr}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	srv.mu.Lock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		srv.mu.Unlock()
	}()
	_, err := srv.callTool(WithCallTimeout(ctx, time.Minute), "ox_merge_deal_domains", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("callTool err = %v; want context.DeadlineExceeded", err)
	}
	if tr.calls != 0 {
		t.Fatalf("transport was called %d time(s) on an expired context", tr.calls)
	}
}
