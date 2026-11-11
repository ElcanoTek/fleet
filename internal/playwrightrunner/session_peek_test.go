package playwrightrunner

import (
	"testing"
	"time"
)

// TestPeekSessionReadsWithoutTouching: PeekSession answers only the owner,
// treats an expired or closed session as gone, and leaves the session's last
// activity alone so a display lookup never keeps a browser alive.
func TestPeekSessionReadsWithoutTouching(t *testing.T) {
	t.Parallel()
	idleSince := time.Now().Add(-time.Minute)
	live := &LiveSession{id: "live", ownerEmail: "alice@example.com", baseURL: "https://a.example.com/login", currentURL: "https://b.example.com/x", lastActive: idleSince}
	expired := &LiveSession{id: "expired", ownerEmail: "alice@example.com", baseURL: "https://a.example.com/", lastActive: time.Now().Add(-sessionIdleTTL - time.Minute)}
	closed := &LiveSession{id: "closed", ownerEmail: "alice@example.com", baseURL: "https://a.example.com/", lastActive: time.Now(), closed: true}
	m := NewSessionManager()
	m.sessions["live"], m.sessions["expired"], m.sessions["closed"] = live, expired, closed

	start, current, ok := m.PeekSession("live", "Alice@Example.com")
	if !ok || start != "https://a.example.com/login" || current != "https://b.example.com/x" {
		t.Errorf("PeekSession(live, owner) = %q %q %v", start, current, ok)
	}
	if !live.lastActive.Equal(idleSince) {
		t.Errorf("PeekSession moved lastActive from %v to %v", idleSince, live.lastActive)
	}
	for _, tc := range []struct{ id, owner string }{
		{"live", "bob@example.com"}, {"live", ""}, {"expired", "alice@example.com"}, {"closed", "alice@example.com"}, {"missing", "alice@example.com"},
	} {
		if _, _, ok := m.PeekSession(tc.id, tc.owner); ok {
			t.Errorf("PeekSession(%q, %q) found a session", tc.id, tc.owner)
		}
	}
	if len(m.sessions) != 3 {
		t.Errorf("PeekSession reaped sessions: %d left", len(m.sessions))
	}
}
