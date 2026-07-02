package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// capturingPush is a PushSender fake recording the events it was handed.
type capturingPush struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (c *capturingPush) SendEvent(_ context.Context, ev Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return c.err
}

func (c *capturingPush) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// TestNotify_PushFanout: a Notifier with ONLY a push backend (no SMTP, no
// webhook — cfg disabled) still fans out to it when the event carries an
// Audience, and skips it when the Audience is empty (no per-user route).
func TestNotify_PushFanout(t *testing.T) {
	push := &capturingPush{}
	n := New(Config{})
	n.SetPush(push)

	ev := sampleEvent()
	ev.Audience = "owner@example.com"
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if push.count() != 1 {
		t.Fatalf("push received %d events, want 1", push.count())
	}
	if got := push.events[0].Audience; got != "owner@example.com" {
		t.Errorf("Audience = %q, want owner@example.com", got)
	}

	// No audience → the push channel is skipped (still no error).
	ev.Audience = "   "
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify (no audience): %v", err)
	}
	if push.count() != 1 {
		t.Errorf("audience-less event reached the push backend")
	}
}

// TestNotify_PushStatusFilterAndErrors: the On-list status filter applies to
// the push channel like every other, and a push failure surfaces in the
// joined error (the runner only logs it) without panicking.
func TestNotify_PushStatusFilterAndErrors(t *testing.T) {
	push := &capturingPush{}
	n := New(Config{On: []string{"failure"}})
	n.SetPush(push)

	ev := sampleEvent() // StatusSuccess — filtered out
	ev.Audience = "owner@example.com"
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if push.count() != 0 {
		t.Error("On-list filter must apply to the push channel")
	}

	push.err = errors.New("relay down")
	ev.Status = StatusFailure
	err := n.Notify(context.Background(), ev)
	if err == nil || push.count() != 1 {
		t.Fatalf("want push error surfaced (events=%d), got err=%v", push.count(), err)
	}

	// A nil push (never set) keeps the disabled-config short-circuit: no fire.
	bare := New(Config{})
	if err := bare.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify (no channels): %v", err)
	}
}
