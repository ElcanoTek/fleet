package mcpoauth

import (
	"errors"
	"fmt"
	"testing"
)

// The terminal set condemns the stored grant or client registration; everything
// else must stay transient so a blip is retried instead of forcing the user
// through a reconnect. HTTPStatus is set on every case to pin that the decision
// keys off the OAuth error code, not the HTTP status.
func TestIsTerminalRefreshError(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
	}{
		{"invalid_grant", true},
		{"invalid_client", true},
		{"unauthorized_client", true},
		{"invalid_scope", false},  // Refresh recovers from this on its own
		{"invalid_target", false}, // Refresh retries without `resource`
		{"invalid_request", false},
		{"temporarily_unavailable", false},
		{"http_500", false},
		{"http_503", false},
	} {
		err := &OAuthError{Code: tc.code, HTTPStatus: 400}
		if got := IsTerminalRefreshError(err); got != tc.want {
			t.Errorf("IsTerminalRefreshError(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}

	// A non-OAuth error (network failure, context cancellation) is never terminal.
	if IsTerminalRefreshError(errors.New("dial tcp: connection refused")) {
		t.Error("a transport error must not be terminal")
	}
	if IsTerminalRefreshError(nil) {
		t.Error("nil must not be terminal")
	}

	// The classification travels through wrapping: callers annotate the error on
	// the way up, and errors.As must still find the OAuthError underneath.
	if !IsTerminalRefreshError(fmt.Errorf("refresh %q: %w", "srv", &OAuthError{Code: "invalid_grant", HTTPStatus: 400})) {
		t.Error("a wrapped terminal error must stay terminal")
	}
	if IsTerminalRefreshError(fmt.Errorf("refresh %q: %w", "srv", &OAuthError{Code: "invalid_target", HTTPStatus: 400})) {
		t.Error("a wrapped transient error must stay transient")
	}
}
