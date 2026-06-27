package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestShutdownGrace resolves the grace period from config and treats a
// non-positive value as "no wait" (0).
func TestShutdownGrace(t *testing.T) {
	cases := []struct {
		secs int
		want time.Duration
	}{
		{30, 30 * time.Second},
		{1, time.Second},
		{0, 0},
		{-5, 0},
	}
	for _, c := range cases {
		got := shutdownGrace(&config.Config{ShutdownGraceSeconds: c.secs})
		if got != c.want {
			t.Errorf("shutdownGrace(%d) = %s, want %s", c.secs, got, c.want)
		}
	}
	if got := shutdownGrace(nil); got != 0 {
		t.Errorf("shutdownGrace(nil) = %s, want 0", got)
	}
}

// TestSDNotify_NoSocketIsNoop pins that sdNotify does nothing (no panic, no
// error surface) when NOTIFY_SOCKET is unset — the non-systemd / dev / test path.
func TestSDNotify_NoSocketIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	sdNotify("READY=1") // must not panic
}

// TestSDNotify_SendsToSocket pins that, when NOTIFY_SOCKET points at a datagram
// socket, the state line is delivered verbatim.
func TestSDNotify_SendsToSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "notify.sock")
	addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Skipf("unixgram not available: %v", err)
	}
	defer conn.Close()

	t.Setenv("NOTIFY_SOCKET", sockPath)
	sdNotify("STOPPING=1")

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "STOPPING=1" {
		t.Errorf("payload = %q, want STOPPING=1", got)
	}
}

// TestCloseServers shuts running listeners down within the budget and returns.
func TestCloseServers(t *testing.T) {
	mk := func() *http.Server {
		ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		t.Cleanup(ts.Close)
		return ts.Config
	}
	s1, s2 := mk(), mk()

	done := make(chan struct{})
	go func() {
		closeServers(500*time.Millisecond, s1, s2)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeServers did not return within the budget")
	}
}

// TestCloseServers_ZeroBudgetFallsBack ensures a non-positive budget still
// produces a bounded Shutdown (no infinite/zero deadline).
func TestCloseServers_ZeroBudgetFallsBack(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(ts.Close)

	done := make(chan struct{})
	go func() {
		closeServers(0, ts.Config)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closeServers(0,...) did not return")
	}
}
