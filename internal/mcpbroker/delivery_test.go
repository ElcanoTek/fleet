package mcpbroker

import (
	"context"
	"errors"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// The child's answer to "did the tool request reach its server?" crosses the
// broker boundary with the masked error, so the parent's caller can tell a
// safe retry from one that might act twice, as it can in-process.
func TestClientServer_NotSentCrossesTheBoundary(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"not sent", mcp.MarkNotSent(errors.New("server retired")), true},
		{"may have been sent", errors.New("read: EOF"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := loopback(t, &fakeBroker{err: tc.err})
			_, _, err := client.CallMCP(context.Background(), "s", "t", nil)
			if err == nil {
				t.Fatal("want a transport error")
			}
			if got := mcp.RequestNotSent(err); got != tc.want {
				t.Fatalf("RequestNotSent(%v) = %v, want %v", err, got, tc.want)
			}
			if mcp.RequestRefused(err) {
				t.Fatalf("RequestRefused(%v) = true, want false", err)
			}
			if err.Error() != errBrokerCallFailed {
				t.Fatalf("err = %q, want the masked text unchanged", err)
			}
		})
	}
}

// A server's 4xx refusal crosses the broker boundary as a refusal, whether
// the error text is masked (a bundle server) or passed (a hosted vendor's
// 4xx), so the parent's caller knows the server did not act. A 5xx does not.
func TestClientServer_RefusedCrossesTheBoundary(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantRefused bool
	}{
		{"bundle server 422", &mcp.HTTPStatusError{StatusCode: 422, Body: "invalid recipient"}, true},
		{"hosted vendor 429", &HostedCallError{Err: &mcp.HTTPStatusError{StatusCode: 429, Body: "rate limited"}}, true},
		{"gateway 502", &mcp.HTTPStatusError{StatusCode: 502, Body: "bad gateway"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := loopback(t, &fakeBroker{err: tc.err})
			_, _, err := client.CallMCP(context.Background(), "s", "t", nil)
			if err == nil {
				t.Fatal("want a transport error")
			}
			if got := mcp.RequestRefused(err); got != tc.wantRefused {
				t.Fatalf("RequestRefused(%v) = %v, want %v", err, got, tc.wantRefused)
			}
			if mcp.RequestNotSent(err) {
				t.Fatalf("RequestNotSent(%v) = true, want false", err)
			}
		})
	}
}

// A call on a closed scope never left the parent.
func TestScope_ClosedScopeCallIsNotSent(t *testing.T) {
	s := &Scope{closed: true}
	_, _, err := s.CallMCP(context.Background(), "s", "t", nil)
	if !errors.Is(err, errScopeClosed) || !mcp.RequestNotSent(err) {
		t.Fatalf("err = %v, want errScopeClosed marked not sent", err)
	}
}
