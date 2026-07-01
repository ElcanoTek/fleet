package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty → %v, want 0", got)
	}
	// 1..100 ms.
	var s []time.Duration
	for i := 1; i <= 100; i++ {
		s = append(s, time.Duration(i)*time.Millisecond)
	}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{50, 50 * time.Millisecond},
		{95, 95 * time.Millisecond},
		{99, 99 * time.Millisecond},
		{100, 100 * time.Millisecond},
		{0, 1 * time.Millisecond},
	}
	for _, c := range cases {
		if got := percentile(s, c.p); got != c.want {
			t.Errorf("percentile(p%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestReadTurnToTerminal(t *testing.T) {
	completed := "id: 1\nevent: turn.started\ndata: {}\n\nevent: text.delta\ndata: {\"text\":\"hi\"}\n\nevent: turn.completed\ndata: {}\n\n"
	if ev, err := readTurnToTerminal(strings.NewReader(completed)); err != nil || ev != "turn.completed" {
		t.Errorf("completed stream → (%q, %v), want (turn.completed, nil)", ev, err)
	}
	errored := "event: text.delta\ndata: {}\n\nevent: turn.error\ndata: {\"message\":\"boom\"}\n\n"
	if ev, err := readTurnToTerminal(strings.NewReader(errored)); err != nil || ev != "turn.error" {
		t.Errorf("errored stream → (%q, %v), want (turn.error, nil)", ev, err)
	}
	truncated := "event: text.delta\ndata: {}\n\n" // no terminal event
	if _, err := readTurnToTerminal(strings.NewReader(truncated)); !errors.Is(err, io.EOF) {
		t.Errorf("truncated stream should return io.EOF, got %v", err)
	}
}

// fakeChatSSE mimics the chat server's POST /chat: it enforces the two required
// headers, then streams a few SSE frames ending in turn.completed.
func fakeChatSSE(t *testing.T) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Chat-Server-Token") == "" || r.Header.Get("X-User-Email") == "" {
			http.Error(w, "missing auth headers", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frames := []string{
			"id: 1\nevent: turn.started\ndata: {}\n\n",
			"id: 2\nevent: text.delta\ndata: {\"text\":\"hi\"}\n\n",
			"id: 3\nevent: turn.completed\ndata: {}\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			if fl != nil {
				fl.Flush()
			}
		}
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunChatLoad drives the concurrent load path against the fake SSE server:
// it must complete turns with no errors and record latencies. Validates the
// post→SSE→terminal→latency + header-auth + concurrency logic without a live fleet.
func TestRunChatLoad(t *testing.T) {
	srv := fakeChatSSE(t)
	opts := chatOptions{
		server:      srv.URL,
		email:       "bench@example.com",
		token:       "test-token",
		message:     "[[echo:hi]] go",
		concurrency: 4,
		duration:    300 * time.Millisecond,
		timeout:     5 * time.Second,
	}
	r := runChatLoad(opts)
	if r.turns == 0 {
		t.Fatal("expected at least one completed turn")
	}
	if r.errors != 0 {
		t.Errorf("expected no errors, got %d", r.errors)
	}
	if len(r.latencies) != int(r.turns) {
		t.Errorf("latencies (%d) should match turns (%d)", len(r.latencies), r.turns)
	}
	if percentile(r.latencies, 50) <= 0 {
		t.Error("p50 latency should be positive")
	}
}

// TestRunChatLoad_ErrorsCounted verifies a non-200 / errored turn is counted as
// an error, not a success.
func TestRunChatLoad_ErrorsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	opts := chatOptions{
		server: srv.URL, email: "b@x.co", token: "t",
		concurrency: 2, duration: 150 * time.Millisecond, timeout: 2 * time.Second,
	}
	r := runChatLoad(opts)
	if r.errors == 0 {
		t.Error("a 500 response should be counted as an error")
	}
	if r.turns != 0 {
		t.Errorf("no turns should succeed, got %d", r.turns)
	}
}

func TestResolveServer(t *testing.T) {
	t.Setenv("FLEET_CHAT_URL", "")
	t.Setenv("FLEET_SERVER_ADDR", "")
	if got := resolveServer(""); got != "http://127.0.0.1:8080" {
		t.Errorf("default server = %q", got)
	}
	if got := resolveServer("http://x:9/"); got != "http://x:9" {
		t.Errorf("flag server = %q (trailing slash should be trimmed)", got)
	}
	t.Setenv("FLEET_SERVER_ADDR", ":8080")
	if got := resolveServer(""); got != "http://127.0.0.1:8080" {
		t.Errorf("addr :8080 → %q", got)
	}
}
