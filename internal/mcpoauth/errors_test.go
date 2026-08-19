package mcpoauth

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsTerminalRefreshError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "non-OAuthError",
			err:  errors.New("some other error"),
			want: false,
		},
		{
			name: "invalid_grant",
			err:  &OAuthError{Code: "invalid_grant"},
			want: true,
		},
		{
			name: "invalid_client",
			err:  &OAuthError{Code: "invalid_client"},
			want: true,
		},
		{
			name: "unauthorized_client",
			err:  &OAuthError{Code: "unauthorized_client"},
			want: true,
		},
		{
			name: "non-terminal code",
			err:  &OAuthError{Code: "invalid_scope"},
			want: false,
		},
		{
			name: "wrapped terminal error",
			err:  fmt.Errorf("wrapped: %w", &OAuthError{Code: "invalid_grant"}),
			want: true,
		},
		{
			name: "wrapped non-terminal error",
			err:  fmt.Errorf("wrapped: %w", &OAuthError{Code: "invalid_target"}),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTerminalRefreshError(tt.err); got != tt.want {
				t.Errorf("IsTerminalRefreshError() = %v, want %v", got, tt.want)
			}
		})
	}
}
