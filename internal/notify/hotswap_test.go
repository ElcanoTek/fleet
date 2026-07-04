package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// SetConfig hot-swap (notifyadmin). Load-bearing assertions: a swap takes
// effect for the NEXT send on the same shared pointer, enablement reads are
// live, and concurrent swap+send is race-clean (exercised under -race).
func TestSetConfigHotSwap(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Boot config: nothing configured — Notify is a no-op.
	n := New(Config{})
	n.logf = func(string, ...any) {}
	if n.Enabled() {
		t.Fatal("empty config must be disabled")
	}
	if err := n.Notify(context.Background(), Event{TaskID: "t", Status: StatusSuccess}); err != nil {
		t.Fatalf("disabled notify: %v", err)
	}
	if hits != 0 {
		t.Fatal("disabled notifier must not send")
	}

	// Swap in a webhook config: the SAME pointer now delivers.
	n.SetConfig(Config{WebhookURL: srv.URL, Retries: -1, Timeout: 2 * time.Second})
	if !n.Enabled() {
		t.Fatal("swapped config should enable the webhook channel")
	}
	if err := n.Notify(context.Background(), Event{TaskID: "t", Status: StatusSuccess}); err != nil {
		t.Fatalf("notify after swap: %v", err)
	}
	if hits != 1 {
		t.Fatalf("swapped-in webhook should deliver, hits=%d", hits)
	}

	// Swap back to empty: disabled again, live.
	n.SetConfig(Config{})
	if n.Enabled() || n.ReplyEnabled() {
		t.Fatal("swap back to empty should disable")
	}
}

// TestSetConfigConcurrent: swaps racing sends — meaningful under `make
// test-race`; sends take one snapshot so a torn config is impossible by
// construction.
func TestSetConfigConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{WebhookURL: srv.URL, Retries: -1, Timeout: 2 * time.Second})
	n.logf = func(string, ...any) {}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			n.SetConfig(Config{WebhookURL: srv.URL, WebhookSecret: "s", Retries: -1, Timeout: 2 * time.Second})
		}()
		go func() {
			defer wg.Done()
			if err := n.Notify(context.Background(), Event{TaskID: "t", Status: StatusSuccess}); err != nil {
				t.Errorf("concurrent notify: %v", err)
			}
		}()
	}
	wg.Wait()
}
