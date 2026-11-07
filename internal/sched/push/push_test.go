// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package push

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeStore serves a fixed work list and records claims.
type fakeStore struct {
	mu     sync.Mutex
	work   []models.A2APushWork
	marked map[string]bool
}

func (f *fakeStore) ListA2APushWork(context.Context, int) ([]models.A2APushWork, error) {
	return f.work, nil
}

func (f *fakeStore) MarkA2APushAttempted(_ context.Context, _ uuid.UUID, id string, _ models.TaskStatus) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.marked[id] {
		return false, nil
	}
	f.marked[id] = true
	return true, nil
}

func (f *fakeStore) isMarked(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marked[id]
}

func pushWork(id, url string) models.A2APushWork {
	return models.A2APushWork{
		Config: models.A2APushConfig{TaskID: uuid.New(), ID: id, URL: url},
		Status: models.TaskStatusRunning,
	}
}

// TestTickSlowReceiverDoesNotDelayOthers pins #2040: a receiver that hangs
// holds one worker, and a fast receiver queued behind it is still delivered
// promptly within the same tick.
func TestTickSlowReceiverDoesNotDelayOthers(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	fastHit := make(chan time.Time, 1)
	fast := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		fastHit <- time.Now()
	}))
	defer fast.Close()

	store := &fakeStore{marked: map[string]bool{}, work: []models.A2APushWork{
		pushWork("slow", slow.URL), pushWork("fast", fast.URL),
	}}
	d := New(store, true) // loopback receivers: dev/TCK posture
	d.logger = slog.New(slog.NewTextHandler(testLogWriter{t}, nil))

	start := time.Now()
	done := make(chan struct{})
	go func() { d.tick(context.Background()); close(done) }()
	select {
	case at := <-fastHit:
		if took := at.Sub(start); took > 2*time.Second {
			t.Fatalf("fast receiver delivered after %s", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast receiver not delivered while the slow one was hanging")
	}
	release <- struct{}{}
	<-done
	if !store.isMarked("slow") || !store.isMarked("fast") {
		t.Fatalf("both deliveries must be claimed: %v", store.marked)
	}
}

// TestTickBudgetDefersUnstartedWork pins the per-tick budget: once it is
// spent, unstarted deliveries are left unclaimed for the next scan rather
// than queued behind slow receivers.
func TestTickBudgetDefersUnstartedWork(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	work := make([]models.A2APushWork, 0, deliverWorkers+3)
	for i := range deliverWorkers + 3 {
		work = append(work, pushWork(string(rune('a'+i)), slow.URL))
	}
	store := &fakeStore{marked: map[string]bool{}, work: work}
	d := New(store, true)
	d.logger = slog.New(slog.NewTextHandler(testLogWriter{t}, nil))
	d.budget = 100 * time.Millisecond

	done := make(chan struct{})
	go func() { d.tick(context.Background()); close(done) }()
	time.Sleep(300 * time.Millisecond) // past the budget, workers all hung
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not finish after its in-flight deliveries")
	}
	claimed := 0
	for _, w := range work {
		if store.isMarked(w.Config.ID) {
			claimed++
		}
	}
	if claimed != deliverWorkers {
		t.Fatalf("claimed %d deliveries, want exactly the %d started before the budget ran out", claimed, deliverWorkers)
	}
}

// testLogWriter hands each slog line to t.Log.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }
