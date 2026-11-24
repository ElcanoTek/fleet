package chattui

import (
	"strings"
	"testing"
)

// A key bound to an earlier account under its email gets its own fix-it text:
// mint a new key for the account — not "add the user", which already exists.
func TestForbiddenMessageKeyPredatesAccount(t *testing.T) {
	t.Parallel()
	got := forbiddenMessage("u@x.com", []byte(`{"error":"key_predates_account","message":"…"}`))
	for _, want := range []string{forbiddenPrefix, "earlier account under u@x.com", "fleet sched apikey create"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "fleet chat user add") {
		t.Errorf("message %q tells the admin to add an account that exists", got)
	}
}
