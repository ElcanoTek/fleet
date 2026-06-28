package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeNotifier captures the events the runner fires so tests can assert payload
// construction + that the right terminal branches fire, without any network.
type fakeNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (f *fakeNotifier) Notify(_ context.Context, ev notify.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeNotifier) drain() []notify.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notify.Event, len(f.events))
	copy(out, f.events)
	return out
}

// TestBuildEvent checks the runner constructs the secret-free event from a task
// + session: prompt truncated to the display name, cost from the session, and an
// absolute log URL built from the configured public base.
func TestBuildEvent(t *testing.T) {
	id := uuid.New()
	longPrompt := strings.Repeat("x", 200)
	task := &models.Task{ID: id, Prompt: longPrompt}
	session := &models.LogSession{Cost: 0.5}

	p := &Pool{publicURLBase: "https://fleet.example.com"}
	ev := p.buildEvent(task, notify.StatusSuccess, session, 90*time.Second)

	if ev.TaskID != id.String() {
		t.Errorf("TaskID = %q, want %q", ev.TaskID, id.String())
	}
	if ev.Status != notify.StatusSuccess {
		t.Errorf("Status = %q, want success", ev.Status)
	}
	if ev.CostUSD != "0.5000" {
		t.Errorf("CostUSD = %q, want 0.5000", ev.CostUSD)
	}
	if ev.DurationSeconds != 90 {
		t.Errorf("DurationSeconds = %d, want 90", ev.DurationSeconds)
	}
	want := "https://fleet.example.com/orchestrator/tasks/" + id.String()
	if ev.LogURL != want {
		t.Errorf("LogURL = %q, want %q", ev.LogURL, want)
	}
	// Name truncated to 60 runes + ellipsis.
	if len([]rune(ev.Name)) != 61 || !strings.HasSuffix(ev.Name, "…") {
		t.Errorf("Name not truncated: len=%d %q", len([]rune(ev.Name)), ev.Name)
	}
}

// TestBuildEvent_NilSessionNoBase covers the nil-session (early failure) and
// no-public-base cases: cost defaults to 0.0000 and the log URL is empty.
func TestBuildEvent_NilSessionNoBase(t *testing.T) {
	p := &Pool{}
	ev := p.buildEvent(&models.Task{ID: uuid.New(), Prompt: "short"}, notify.StatusFailure, nil, time.Second)
	if ev.CostUSD != "0.0000" {
		t.Errorf("CostUSD = %q, want 0.0000", ev.CostUSD)
	}
	if ev.LogURL != "" {
		t.Errorf("LogURL = %q, want empty (no public base)", ev.LogURL)
	}
	if ev.Name != "short" {
		t.Errorf("Name = %q, want short", ev.Name)
	}
}

// TestNotifyTerminal_Fires checks the wired notifier receives an event, and that
// a nil notifier (default OFF) is a safe no-op.
func TestNotifyTerminal_Fires(t *testing.T) {
	fake := &fakeNotifier{}
	p := &Pool{notifier: fake, publicURLBase: "https://x.example"}
	task := &models.Task{ID: uuid.New(), Prompt: "p"}

	p.notifyTerminal(task, notify.StatusFailure, &models.LogSession{Cost: 1.25}, 5*time.Second)

	// notifyTerminal fires from a detached goroutine; wait for the event.
	deadline := time.Now().Add(2 * time.Second)
	for len(fake.drain()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("notifier was not fired within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ev := fake.drain()[0]
	if ev.Status != notify.StatusFailure || ev.CostUSD != "1.2500" {
		t.Errorf("unexpected event: %+v", ev)
	}

	// nil notifier: no panic, no fire.
	pNil := &Pool{}
	pNil.notifyTerminal(task, notify.StatusSuccess, nil, time.Second) // must not panic
}
