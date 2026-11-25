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
			if err.Error() != errBrokerCallFailed {
				t.Fatalf("err = %q, want the masked text unchanged", err)
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
