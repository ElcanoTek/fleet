package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// TestHasClass is ported from cutlass; it exercises the shared CSS-class
// matcher used by the chat-base web_search.go DuckDuckGo parser.
func TestHasClass(t *testing.T) {
	tests := []struct {
		name      string
		classAttr string
		target    string
		want      bool
	}{
		{"Exact match", "foo", "foo", true},
		{"Multiple classes start", "foo bar baz", "foo", true},
		{"Multiple classes middle", "foo bar baz", "bar", true},
		{"Multiple classes end", "foo bar baz", "baz", true},
		{"Partial match start", "foobar", "foo", false},
		{"Partial match end", "foobar", "bar", false},
		{"Partial match middle", "foobar", "oba", false},
		{"Surrounded by spaces", "  foo  ", "foo", true},
		{"Tabs and newlines", "foo\tbar\nbaz", "bar", true},
		{"No class attribute", "", "foo", false},
		{"Empty class attribute", "", "foo", false},
		{"Case sensitive", "Foo", "foo", false},
		{"Target in longer string", "class-foo", "foo", false},
		{"Target contains hyphen", "btn-primary", "btn-primary", true},
		{"Mixed whitespace", " \t\n\r\fclass1 \f\r\n\t class2", "class2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &html.Node{
				Type: html.ElementNode,
				Data: "div",
				Attr: []html.Attribute{
					{Key: "class", Val: tt.classAttr},
				},
			}
			if tt.classAttr == "" && tt.name == "No class attribute" {
				node.Attr = nil
			}

			if got := hasClass(node, tt.target); got != tt.want {
				t.Errorf("hasClass() = %v, want %v (attr: %q, target: %q)", got, tt.want, tt.classAttr, tt.target)
			}
		})
	}
}

// TestRateLimiterWaitCancelDuringIntervalWait is the regression test for
// issue #561: cancelling the context while a call is blocked in the
// min-interval wait used to return with the mutex already unlocked, so the
// deferred Unlock fired a fatal "sync: unlock of unlocked mutex" that killed
// the whole process. The cancelled call must return ctx.Err(), and the
// limiter must remain usable afterwards.
func TestRateLimiterWaitCancelDuringIntervalWait(t *testing.T) {
	rl := newRateLimiter(10 * time.Second)

	// First call: lastRequest is zero, so it records a request and returns
	// immediately. The second call then owes the full min interval.
	if err := rl.wait(context.Background(), "search"); err != nil {
		t.Fatalf("first wait: unexpected error: %v", err)
	}

	// Second call blocks in the interval select; cancel it mid-wait.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rl.wait(ctx, "search") }()
	time.Sleep(50 * time.Millisecond) // let the goroutine reach the select
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait: got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled wait did not return")
	}

	// The mutex must still be held exactly once per critical section: a
	// fresh cancelled call must be able to lock, block, and bail out again.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := rl.wait(ctx2, "search"); !errors.Is(err, context.Canceled) {
		t.Fatalf("second cancelled wait: got %v, want context.Canceled", err)
	}
}
